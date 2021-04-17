package runner

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/logrusorgru/aurora"
	"github.com/pkg/errors"

	"github.com/projectdiscovery/clistats"
	// automatic fd max increase if running as root
	_ "github.com/projectdiscovery/fdmax/autofdmax"
	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/hmap/store/hybrid"
	customport "github.com/projectdiscovery/httpx/common/customports"
	"github.com/projectdiscovery/httpx/common/fileutil"
	"github.com/projectdiscovery/httpx/common/httputilz"
	"github.com/cyal1/httpx/common/httpx"
	"github.com/projectdiscovery/httpx/common/iputil"
	"github.com/projectdiscovery/httpx/common/slice"
	"github.com/projectdiscovery/httpx/common/stringz"
	"github.com/projectdiscovery/mapcidr"
	"github.com/projectdiscovery/rawhttp"
	wappalyzer "github.com/projectdiscovery/wappalyzergo"
	"github.com/remeh/sizedwaitgroup"
)

const (
	statsDisplayInterval = 5
)

// Runner is a client for running the enumeration process.
type Runner struct {
	options    *Options
	hp         *httpx.HTTPX
	wappalyzer *wappalyzer.Wappalyze
	scanopts   scanOptions
	hm         *hybrid.HybridMap
	stats      clistats.StatisticsClient
}

// New creates a new client for running enumeration process.
func New(options *Options) (*Runner, error) {
	runner := &Runner{
		options: options,
	}
	var err error
	if options.TechDetect {
		runner.wappalyzer, err = wappalyzer.New()
	}
	if err != nil {
		return nil, errors.Wrap(err, "could not create wappalyzer client")
	}

	httpxOptions := httpx.DefaultOptions
	httpxOptions.TLSGrab = options.TLSGrab
	httpxOptions.Timeout = time.Duration(options.Timeout) * time.Second
	httpxOptions.RetryMax = options.Retries
	httpxOptions.FollowRedirects = options.FollowRedirects
	httpxOptions.FollowHostRedirects = options.FollowHostRedirects
	httpxOptions.HTTPProxy = options.HTTPProxy
	httpxOptions.Unsafe = options.Unsafe
	httpxOptions.RequestOverride = httpx.RequestOverride{URIPath: options.RequestURI}
	httpxOptions.CdnCheck = options.OutputCDN
	httpxOptions.RandomAgent = options.RandomAgent

	var key, value string
	httpxOptions.CustomHeaders = make(map[string]string)
	for _, customHeader := range options.CustomHeaders {
		tokens := strings.SplitN(customHeader, ":", two)
		// rawhttp skips all checks
		if options.Unsafe {
			httpxOptions.CustomHeaders[customHeader] = ""
			continue
		}

		// Continue normally
		if len(tokens) < two {
			continue
		}
		key = strings.TrimSpace(tokens[0])
		value = strings.TrimSpace(tokens[1])
		httpxOptions.CustomHeaders[key] = value
	}

	runner.hp, err = httpx.New(&httpxOptions)
	if err != nil {
		gologger.Fatal().Msgf("Could not create httpx instance: %s\n", err)
	}

	var scanopts scanOptions

	if options.InputRawRequest != "" {
		var rawRequest []byte
		rawRequest, err = ioutil.ReadFile(options.InputRawRequest)
		if err != nil {
			gologger.Fatal().Msgf("Could not read raw request from '%s': %s\n", options.InputRawRequest, err)
		}

		rrMethod, rrPath, rrHeaders, rrBody, errParse := httputilz.ParseRequest(string(rawRequest), options.Unsafe)
		if errParse != nil {
			gologger.Fatal().Msgf("Could not parse raw request: %s\n", err)
		}
		scanopts.Methods = append(scanopts.Methods, rrMethod)
		scanopts.RequestURI = rrPath
		for name, value := range rrHeaders {
			httpxOptions.CustomHeaders[name] = value
		}
		scanopts.RequestBody = rrBody
		options.rawRequest = string(rawRequest)
	}

	// disable automatic host header for rawhttp if manually specified
	// as it can be malformed the best approach is to remove spaces and check for lowercase "host" word
	if options.Unsafe {
		for name := range runner.hp.CustomHeaders {
			nameLower := strings.TrimSpace(strings.ToLower(name))
			if strings.HasPrefix(nameLower, "host") {
				rawhttp.AutomaticHostHeader(false)
			}
		}
	}
	if strings.EqualFold(options.Methods, "all") {
		scanopts.Methods = httputilz.AllHTTPMethods()
	} else if options.Methods != "" {
		scanopts.Methods = append(scanopts.Methods, stringz.SplitByCharAndTrimSpace(options.Methods, ",")...)
	}
	if len(scanopts.Methods) == 0 {
		scanopts.Methods = append(scanopts.Methods, http.MethodGet)
	}
	runner.options.protocol = httpx.HTTPorHTTPS
	scanopts.VHost = options.VHost
	scanopts.OutputTitle = options.ExtractTitle
	scanopts.OutputStatusCode = options.StatusCode
	scanopts.OutputLocation = options.Location
	scanopts.OutputContentLength = options.ContentLength
	scanopts.StoreResponse = options.StoreResponse
	scanopts.StoreResponseDirectory = options.StoreResponseDir
	scanopts.OutputServerHeader = options.OutputServerHeader
	scanopts.OutputWithNoColor = options.NoColor
	scanopts.ResponseInStdout = options.responseInStdout
	scanopts.OutputWebSocket = options.OutputWebSocket
	scanopts.TLSProbe = options.TLSProbe
	scanopts.CSPProbe = options.CSPProbe
	if options.RequestURI != "" {
		scanopts.RequestURI = options.RequestURI
	}
	scanopts.VHostInput = options.VHostInput
	scanopts.OutputContentType = options.OutputContentType
	scanopts.RequestBody = options.RequestBody
	scanopts.Unsafe = options.Unsafe
	scanopts.Pipeline = options.Pipeline
	scanopts.HTTP2Probe = options.HTTP2Probe
	scanopts.OutputMethod = options.OutputMethod
	scanopts.OutputIP = options.OutputIP
	scanopts.OutputCName = options.OutputCName
	scanopts.OutputCDN = options.OutputCDN
	scanopts.OutputResponseTime = options.OutputResponseTime
	scanopts.NoFallback = options.NoFallback
	scanopts.TechDetect = options.TechDetect

	// output verb if more than one is specified
	if len(scanopts.Methods) > 1 && !options.Silent {
		scanopts.OutputMethod = true
	}

	runner.scanopts = scanopts

	if options.ShowStatistics {
		runner.stats, err = clistats.New()
		if err != nil {
			return nil, err
		}
	}

	hm, err := hybrid.New(hybrid.DefaultDiskOptions)
	if err != nil {
		return nil, err
	}
	runner.hm = hm

	return runner, nil
}

func (r *Runner) prepareInput() {
	var (
		finput  *os.File
		scanner *bufio.Scanner
		err     error
	)
	// check if file has been provided
	if fileutil.FileExists(r.options.InputFile) {
		finput, err = os.Open(r.options.InputFile)
		if err != nil {
			gologger.Fatal().Msgf("Could read input file '%s': %s\n", r.options.InputFile, err)
		}
		scanner = bufio.NewScanner(finput)
	} else if fileutil.HasStdin() {
		scanner = bufio.NewScanner(os.Stdin)
	} else {
		gologger.Fatal().Msgf("No input provided")
	}

	// Check if the user requested multiple paths
	if fileutil.FileExists(r.options.RequestURIs) {
		r.options.requestURIs = fileutil.LoadFile(r.options.RequestURIs)
	} else if r.options.RequestURIs != "" {
		r.options.requestURIs = strings.Split(r.options.RequestURIs, ",")
	}

	numTargets := 0
	for scanner.Scan() {
		target := strings.TrimSpace(scanner.Text())
		// Used just to get the exact number of targets
		if _, ok := r.hm.Get(target); ok {
			continue
		}

		if len(r.options.requestURIs) > 0 {
			numTargets += len(r.options.requestURIs)
		} else {
			numTargets++
		}
		r.hm.Set(target, nil) //nolint
	}

	if r.options.InputFile != "" {
		err := finput.Close()
		if err != nil {
			gologger.Fatal().Msgf("Could close input file '%s': %s\n", r.options.InputFile, err)
		}
	}

	if r.options.ShowStatistics {
		numPorts := len(customport.Ports)
		if numPorts == 0 {
			// Default Ports 80, 443
			numPorts = 2
		}

		r.stats.AddStatic("hosts", numTargets)
		r.stats.AddStatic("startedAt", time.Now())
		r.stats.AddCounter("requests", 0)
		r.stats.AddCounter("total", uint64(numTargets*numPorts))
		err := r.stats.Start(makePrintCallback(), time.Duration(statsDisplayInterval)*time.Second)
		if err != nil {
			gologger.Warning().Msgf("Could not create statistic: %s\n", err)
		}
	}
}

func makePrintCallback() func(stats clistats.StatisticsClient) {
	builder := &strings.Builder{}
	return func(stats clistats.StatisticsClient) {
		builder.WriteRune('[')
		startedAt, _ := stats.GetStatic("startedAt")
		duration := time.Since(startedAt.(time.Time))
		builder.WriteString(clistats.FmtDuration(duration))
		builder.WriteRune(']')

		hosts, _ := stats.GetStatic("hosts")
		builder.WriteString(" | Hosts: ")
		builder.WriteString(clistats.String(hosts))

		requests, _ := stats.GetCounter("requests")
		total, _ := stats.GetCounter("total")

		builder.WriteString(" | RPS: ")
		builder.WriteString(clistats.String(uint64(float64(requests) / duration.Seconds())))

		builder.WriteString(" | Requests: ")
		builder.WriteString(clistats.String(requests))
		builder.WriteRune('/')
		builder.WriteString(clistats.String(total))
		builder.WriteRune(' ')
		builder.WriteRune('(')
		//nolint:gomnd // this is not a magic number
		builder.WriteString(clistats.String(uint64(float64(requests) / float64(total) * 100.0)))
		builder.WriteRune('%')
		builder.WriteRune(')')
		builder.WriteRune('\n')

		fmt.Fprintf(os.Stderr, "%s", builder.String())
		builder.Reset()
	}
}

// Close closes the httpx scan instance
func (r *Runner) Close() {
	// nolint:errcheck // ignore
	r.hm.Close()
	r.hp.Dialer.Close()
}

// RunEnumeration on targets for httpx client
func (r *Runner) RunEnumeration() {
	// Try to create output folder if it doesnt exist
	if r.options.StoreResponse && !fileutil.FolderExists(r.options.StoreResponseDir) {
		if err := os.MkdirAll(r.options.StoreResponseDir, os.ModePerm); err != nil {
			gologger.Fatal().Msgf("Could not create output directory '%s': %s\n", r.options.StoreResponseDir, err)
		}
	}

	r.prepareInput()

	// output routine
	wgoutput := sizedwaitgroup.New(1)
	wgoutput.Add()
	output := make(chan Result)
	go func(output chan Result) {
		defer wgoutput.Done()

		var f *os.File
		if r.options.Output != "" {
			var err error
			f, err = os.Create(r.options.Output)
			if err != nil {
				gologger.Fatal().Msgf("Could not create output file '%s': %s\n", r.options.Output, err)
			}
			if r.options.HTMLOutput{
				f.WriteString(`
<script>
function onSearch(filed,contain){
    var oEl = document.getElementById('test');
    var oInp = document.getElementById('inp');
    var c = 0;
        setTimeout(function(){   
            var rows = oEl.rows.length;
            var inpVal = oInp.value;
            if (inpVal.trim()!=""){
                    for(var i=1;i<rows;i++){
                    var cellText = oEl.rows[i].cells[filed].innerHTML;//cells[2] is Status code
                    if (contain == "1"){
                        if(cellText.includes(inpVal)){ 
                            c++;
                            oEl.rows[i].style.display='';   
                        }else{
                            oEl.rows[i].style.display='none';
                        }
                    }else{
                        if(!cellText.includes(inpVal)){ 
                        c++;
                        oEl.rows[i].style.display='';   
                        }else{
                            oEl.rows[i].style.display='none';
                        }
                    }
                     
                }
            }else{
               for(var i=1;i<rows;i++){
                     c++;
                     oEl.rows[i].style.display='';   
                }
            }
            var count = document.getElementById('count');
            count.innerHTML=" "+c+" rows ";
        },200)
    console.log(oEl.rows.length-1);
}
window.onload = function(){
    onSearch();
    var thr = document.getElementById('test');
    var slt = document.getElementsByTagName("select")[0];
    var opt="";
    for(var i =0;i<thr.rows[0].cells.length;i++){
        opt += " <option value ="+i+">" + thr.rows[0].cells[i].innerHTML + "</option>"
    }
    slt.innerHTML=opt;

}

function fresh(){
    onSearch(document.getElementsByTagName('select')[0].selectedOptions[0].value,document.getElementsByTagName('select')[1].selectedOptions[0].value);
}

</script>
<style>
    input,select{
        border: 1px solid #C1DAD7; 
        padding: 4px 0px;
        border-radius: 3px;
        padding-left:5px; 
    }
    *{
        padding: 0;
        margin: 0;
        border: none;
    }
    body {
        font: normal 11px  "Trebuchet MS", Verdana, Arial, Helvetica, sans-serif;
        color: #4f6b72;
    }
    a {
        color: #c75f3e;
    }
    table{
        margin: 0 auto;
        margin-top: 35px;
        border-collapse: collapse;
        border: 1px solid #C1DAD7;
    }
    tr:nth-child(odd){
        background-color: #eeeeee;
    }
    tr:hover{
        background-color: #fffeee;
    }
    td,th {
        border-right: 1px solid #C1DAD7;
        border-top: 1px solid #C1DAD7;
        font-size:13px;
        padding: 6px 6px 0px 12px;
        color: #4f6b72;
        white-space: nowrap;
        max-width: 300px;
        overflow: hidden;
        text-overflow: ellipsis;
    }
    #filter-bar{
        padding: 2px 25px;
        position: fixed;
        top:0px;
        background-color: #C1DAD7;
        opacity: 0.9;
        width: 100%;
        box-shadow: 0 0 1px 0px rgb(0 0 0 / 30%), 0 0 6px 2px rgb(0 0 0 / 15%);
    }
</style>

<div id="filter-bar">
<span>
<select onchange="fresh()">
</select>
</span>
<span>
    <select onchange="fresh()">
    <option value="1">包含</option>
    <option value="0">不包含</option>
    </select>
</span>
<span><input type="text" id="inp" value="" oninput="fresh()" /></span>
<span id="count"><span>
</div>
<table id="test"><th>URL</th><th>IP</th><th>Status Code</th><th>Content-Length</th><th>Title</th><th>Technology</th><th>Web Server</th><th>Location</th>`)
			}
			defer f.Close() //nolint
		}
		for resp := range output {
			if resp.err != nil {
				gologger.Debug().Msgf("Failure '%s': %s\n", resp.URL, resp.err)
			}
			if resp.str == "" {
				continue
			}

			// apply matchers and filters
			if len(r.options.filterStatusCode) > 0 && slice.IntSliceContains(r.options.filterStatusCode, resp.StatusCode) {
				continue
			}
			if len(r.options.filterContentLength) > 0 && slice.IntSliceContains(r.options.filterContentLength, resp.ContentLength) {
				continue
			}
			if r.options.filterRegex != nil && r.options.filterRegex.MatchString(resp.raw) {
				continue
			}
			if r.options.OutputFilterString != "" && strings.Contains(strings.ToLower(resp.raw), strings.ToLower(r.options.OutputFilterString)) {
				continue
			}
			if len(r.options.matchStatusCode) > 0 && !slice.IntSliceContains(r.options.matchStatusCode, resp.StatusCode) {
				continue
			}
			if len(r.options.matchContentLength) > 0 && !slice.IntSliceContains(r.options.matchContentLength, resp.ContentLength) {
				continue
			}
			if r.options.matchRegex != nil && !r.options.matchRegex.MatchString(resp.raw) {
				continue
			}
			if r.options.OutputMatchString != "" && !strings.Contains(strings.ToLower(resp.raw), strings.ToLower(r.options.OutputMatchString)) {
				continue
			}

			row := resp.str
			if r.options.JSONOutput {
				row = resp.JSON()
			}
			gologger.Silent().Msgf("%s\n", row)
			if f != nil {
				//nolint:errcheck // this method needs a small refactor to reduce complexity
				if r.options.HTMLOutput {
					u := td("<a href='" + resp.URL + "' title='" + resp.URL + "' target='_blank'>" + resp.URL + "</a>")
					ip := strings.Replace(strings.Trim(fmt.Sprint(resp.Host), "[]"), " ", ", ", -1)
					ip = "<td title='" + ip + "'>" + ip + "</td>"
					status := td(strconv.Itoa(resp.StatusCode))
					cl := td(strconv.Itoa(resp.ContentLength))
					title := "<td title='" + resp.Title + "'>" + resp.Title + "</td>"
					tech :="<td title='" + strings.Join(resp.Technologies,",") + "'>" + strings.Join(resp.Technologies,",") + "</td>"
					ws := "<td title='" + resp.WebServer + "'>" + resp.WebServer + "</td>"
					location:= td(resp.Location)
					tr := "<tr>" + u + ip + status + cl + title + tech + ws + location + "</tr>"
					f.WriteString(tr + "\n")
				} else {
					f.WriteString(row + "\n")
				}
			}
		}
	}(output)

	wg := sizedwaitgroup.New(r.options.Threads)

	r.hm.Scan(func(k, _ []byte) error {
		var reqs int
		if len(r.options.requestURIs) > 0 {
			for _, p := range r.options.requestURIs {
				scanopts := r.scanopts.Clone()
				scanopts.RequestURI = p
				r.process(string(k), &wg, r.hp, r.options.protocol, scanopts, output)
				reqs++
			}
		} else {
			r.process(string(k), &wg, r.hp, r.options.protocol, &r.scanopts, output)
			reqs++
		}

		if r.options.ShowStatistics {
			r.stats.IncrementCounter("requests", reqs)
		}
		return nil
	})

	wg.Wait()

	close(output)

	wgoutput.Wait()
}

func (r *Runner) process(t string, wg *sizedwaitgroup.SizedWaitGroup, hp *httpx.HTTPX, protocol string, scanopts *scanOptions, output chan Result) {
	protocols := []string{protocol}
	if scanopts.NoFallback {
		protocols = []string{httpx.HTTPS, httpx.HTTP}
	}
	for target := range targets(stringz.TrimProtocol(t)) {
		// if no custom ports specified then test the default ones
		if len(customport.Ports) == 0 {
			for _, method := range scanopts.Methods {
				for _, prot := range protocols {
					wg.Add()
					go func(target, method, protocol string) {
						defer wg.Done()
						result := r.analyze(hp, protocol, target, 0, method, scanopts)
						output <- result
						if scanopts.TLSProbe && result.TLSData != nil {
							scanopts.TLSProbe = false
							for _, tt := range result.TLSData.DNSNames {
								r.process(tt, wg, hp, protocol, scanopts, output)
							}
							for _, tt := range result.TLSData.CommonName {
								r.process(tt, wg, hp, protocol, scanopts, output)
							}
						}
						if scanopts.CSPProbe && result.CSPData != nil {
							scanopts.CSPProbe = false
							for _, tt := range result.CSPData.Domains {
								r.process(tt, wg, hp, protocol, scanopts, output)
							}
						}
					}(target, method, prot)
				}
			}
		}

		// the host name shouldn't have any semicolon - in case remove the port
		semicolonPosition := strings.LastIndex(target, ":")
		if semicolonPosition > 0 {
			target = target[:semicolonPosition]
		}

		for port, wantedProtocol := range customport.Ports {
			for _, method := range scanopts.Methods {
				wg.Add()
				go func(port int, method, protocol string) {
					defer wg.Done()
					result := r.analyze(hp, protocol, target, port, method, scanopts)
					output <- result
					if scanopts.TLSProbe && result.TLSData != nil {
						scanopts.TLSProbe = false
						for _, tt := range result.TLSData.DNSNames {
							r.process(tt, wg, hp, protocol, scanopts, output)
						}
						for _, tt := range result.TLSData.CommonName {
							r.process(tt, wg, hp, protocol, scanopts, output)
						}
					}
				}(port, method, wantedProtocol)
			}
		}
	}
}

// returns all the targets within a cidr range or the single target
func targets(target string) chan string {
	results := make(chan string)
	go func() {
		defer close(results)

		// A valid target does not contain:
		// *
		// spaces
		if strings.ContainsAny(target, " *") {
			return
		}

		// test if the target is a cidr
		if iputil.IsCidr(target) {
			cidrIps, err := mapcidr.IPAddresses(target)
			if err != nil {
				return
			}
			for _, ip := range cidrIps {
				results <- ip
			}
		} else {
			results <- target
		}
	}()
	return results
}

func (r *Runner) analyze(hp *httpx.HTTPX, protocol, domain string, port int, method string, scanopts *scanOptions) Result {
	origProtocol := protocol
	if protocol == httpx.HTTPorHTTPS {
		protocol = httpx.HTTPS
	}
	retried := false
retry:
	var customHost string
	if scanopts.VHostInput {
		parts := strings.Split(domain, ",")
		//nolint:gomnd // not a magic number
		if len(parts) != 2 {
			return Result{}
		}
		domain = parts[0]
		customHost = parts[1]
	}

	URL := fmt.Sprintf("%s://%s", protocol, domain)
	if port > 0 {
		URL = fmt.Sprintf("%s://%s:%d", protocol, domain, port)
	}

	if !scanopts.Unsafe {
		URL += scanopts.RequestURI
	}

	req, err := hp.NewRequest(method, URL)
	if err != nil {
		return Result{URL: URL, err: err}
	}
	if customHost != "" {
		req.Host = customHost
	}

	hp.SetCustomHeaders(req, hp.CustomHeaders)
	if scanopts.RequestBody != "" {
		req.ContentLength = int64(len(scanopts.RequestBody))
		req.Body = ioutil.NopCloser(strings.NewReader(scanopts.RequestBody))
	}

	// Create a copy on the fly of the request body - ignore errors
	bodyBytes, _ := req.BodyBytes()
	req.Request.Body = ioutil.NopCloser(bytes.NewReader(bodyBytes))
	requestDump, err := httputil.DumpRequestOut(req.Request, true)
	if err != nil {
		return Result{URL: URL, err: err}
	}

	resp, err := hp.Do(req)
	if err != nil {
		if !retried && origProtocol == httpx.HTTPorHTTPS {
			if protocol == httpx.HTTPS {
				protocol = httpx.HTTP
			} else {
				protocol = httpx.HTTPS
			}
			retried = true
			goto retry
		}
		return Result{URL: URL, err: err}
	}

	var fullURL string

	if resp.StatusCode >= 0 {
		if port > 0 {
			fullURL = fmt.Sprintf("%s://%s:%d%s", protocol, domain, port, scanopts.RequestURI)
		} else {
			fullURL = fmt.Sprintf("%s://%s%s", protocol, domain, scanopts.RequestURI)
		}
	}

	builder := &strings.Builder{}

	builder.WriteString(fullURL)

	if scanopts.OutputStatusCode {
		builder.WriteString(" [")
		if !scanopts.OutputWithNoColor {
			// Color the status code based on its value
			switch {
			case resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices:
				builder.WriteString(aurora.Green(strconv.Itoa(resp.StatusCode)).String())
			case resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest:
				builder.WriteString(aurora.Yellow(strconv.Itoa(resp.StatusCode)).String())
			case resp.StatusCode >= http.StatusBadRequest && resp.StatusCode < http.StatusInternalServerError:
				builder.WriteString(aurora.Red(strconv.Itoa(resp.StatusCode)).String())
			case resp.StatusCode > http.StatusInternalServerError:
				builder.WriteString(aurora.Bold(aurora.Yellow(strconv.Itoa(resp.StatusCode))).String())
			}
		} else {
			builder.WriteString(strconv.Itoa(resp.StatusCode))
		}
		builder.WriteRune(']')
	}

	if scanopts.OutputLocation {
		builder.WriteString(" [")
		if !scanopts.OutputWithNoColor {
			builder.WriteString(aurora.Magenta(resp.GetHeaderPart("Location", ";")).String())
		} else {
			builder.WriteString(resp.GetHeaderPart("Location", ";"))
		}
		builder.WriteRune(']')
	}

	if scanopts.OutputMethod {
		builder.WriteString(" [")
		if !scanopts.OutputWithNoColor {
			builder.WriteString(aurora.Magenta(method).String())
		} else {
			builder.WriteString(method)
		}
		builder.WriteRune(']')
	}

	if scanopts.OutputContentLength {
		builder.WriteString(" [")
		if !scanopts.OutputWithNoColor {
			builder.WriteString(aurora.Magenta(strconv.Itoa(resp.ContentLength)).String())
		} else {
			builder.WriteString(strconv.Itoa(resp.ContentLength))
		}
		builder.WriteRune(']')
	}

	if scanopts.OutputContentType {
		builder.WriteString(" [")
		if !scanopts.OutputWithNoColor {
			builder.WriteString(aurora.Magenta(resp.GetHeaderPart("Content-Type", ";")).String())
		} else {
			builder.WriteString(resp.GetHeaderPart("Content-Type", ";"))
		}
		builder.WriteRune(']')
	}

	title := httpx.ExtractTitle(resp)
	if scanopts.OutputTitle {
		builder.WriteString(" [")
		if !scanopts.OutputWithNoColor {
			builder.WriteString(aurora.Cyan(title).String())
		} else {
			builder.WriteString(title)
		}
		builder.WriteRune(']')
	}

	serverHeader := resp.GetHeader("Server")
	if scanopts.OutputServerHeader {
		builder.WriteString(fmt.Sprintf(" [%s]", serverHeader))
	}

	var serverResponseRaw string
	var request string
	var responseHeader string
	if scanopts.ResponseInStdout {
		serverResponseRaw = string(resp.Data)
		request = string(requestDump)
		responseHeader = resp.RawHeaders
	}

	// check for virtual host
	isvhost := false
	if scanopts.VHost {
		isvhost, _ = hp.IsVirtualHost(req)
		if isvhost {
			builder.WriteString(" [vhost]")
		}
	}

	// web socket
	isWebSocket := resp.StatusCode == 101
	if scanopts.OutputWebSocket && isWebSocket {
		builder.WriteString(" [websocket]")
	}

	pipeline := false
	if scanopts.Pipeline {
		pipeline = hp.SupportPipeline(protocol, method, domain, port)
		if pipeline {
			builder.WriteString(" [pipeline]")
		}
	}

	var http2 bool
	// if requested probes for http2
	if scanopts.HTTP2Probe {
		http2 = hp.SupportHTTP2(protocol, method, URL)
		if http2 {
			builder.WriteString(" [http2]")
		}
	}

	ip := hp.Dialer.GetDialedIP(domain)
	if scanopts.OutputIP {
		builder.WriteString(fmt.Sprintf(" [%s]", ip))
	}

	var (
		ips    []string
		cnames []string
	)
	dnsData, err := hp.Dialer.GetDNSData(domain)
	if dnsData != nil && err == nil {
		ips = append(ips, dnsData.A...)
		ips = append(ips, dnsData.AAAA...)
		cnames = dnsData.CNAME
	} else {
		ips = append(ips, ip)
	}

	if scanopts.OutputCName && len(cnames) > 0 {
		// Print only the first CNAME (full list in json)
		builder.WriteString(fmt.Sprintf(" [%s]", cnames[0]))
	}

	isCDN, err := hp.CdnCheck(ip)
	if scanopts.OutputCDN && isCDN && err == nil {
		builder.WriteString(" [cdn]")
	}

	if scanopts.OutputResponseTime {
		builder.WriteString(fmt.Sprintf(" [%s]", resp.Duration))
	}

	var technologies []string
	if scanopts.TechDetect {
		matches := r.wappalyzer.Fingerprint(resp.Headers, resp.Data)
		for match := range matches {
			technologies = append(technologies, match)
		}

		if len(technologies) > 0 {
			technologies := strings.Join(technologies, ",")

			builder.WriteString(" [")
			if !scanopts.OutputWithNoColor {
				builder.WriteString(aurora.Magenta(technologies).String())
			} else {
				builder.WriteString(technologies)
			}
			builder.WriteRune(']')
		}
	}

	// store responses in directory
	if scanopts.StoreResponse {
		domainFile := fmt.Sprintf("%s%s", domain, scanopts.RequestURI)
		if port > 0 {
			domainFile = fmt.Sprintf("%s.%d%s", domain, port, scanopts.RequestURI)
		}
		// On various OS the file max file name length is 255 - https://serverfault.com/questions/9546/filename-length-limits-on-linux
		// Truncating length at 255
		if len(domainFile) >= maxFileNameLength {
			// leaving last 4 bytes free to append ".txt"
			domainFile = domainFile[:maxFileNameLength-1]
		}

		domainFile = strings.ReplaceAll(domainFile, "/", "_") + ".txt"
		responsePath := path.Join(scanopts.StoreResponseDirectory, domainFile)
		writeErr := ioutil.WriteFile(responsePath, []byte(resp.Raw), 0644)
		if writeErr != nil {
			gologger.Warning().Msgf("Could not write response, at path '%s', to disk: %s", responsePath, writeErr)
		}
	}

	parsed, err := url.Parse(fullURL)
	if err != nil {
		return Result{URL: URL, err: errors.Wrap(err, "could not parse url")}
	}

	finalPort := parsed.Port()
	if finalPort == "" {
		if parsed.Scheme == "http" {
			finalPort = "80"
		} else {
			finalPort = "443"
		}
	}
	finalPath := parsed.Path
	if finalPath == "" {
		finalPath = "/"
	}

	hasher := sha256.New()
	_, _ = hasher.Write(resp.Data)
	bodySha := hex.EncodeToString(hasher.Sum(nil))
	hasher.Reset()

	_, _ = hasher.Write([]byte(resp.RawHeaders))
	headersSha := hex.EncodeToString(hasher.Sum(nil))

	return Result{
		Timestamp:      time.Now(),
		Request:        request,
		ResponseHeader: responseHeader,
		Scheme:         parsed.Scheme,
		Port:           finalPort,
		Path:           finalPath,
		BodySHA256:     bodySha,
		HeaderSHA256:   headersSha,
		raw:            resp.Raw,
		URL:            fullURL,
		ContentLength:  resp.ContentLength,
		StatusCode:     resp.StatusCode,
		Location:       resp.GetHeaderPart("Location", ";"),
		ContentType:    resp.GetHeaderPart("Content-Type", ";"),
		Title:          title,
		str:            builder.String(),
		VHost:          isvhost,
		WebServer:      serverHeader,
		ResponseBody:   serverResponseRaw,
		WebSocket:      isWebSocket,
		TLSData:        resp.TLSData,
		CSPData:        resp.CSPData,
		Pipeline:       pipeline,
		HTTP2:          http2,
		Method:         method,
		Host:           ip,
		A:              ips,
		CNAMEs:         cnames,
		CDN:            isCDN,
		ResponseTime:   resp.Duration.String(),
		Technologies:   technologies,
	}
}

// Result of a scan
type Result struct {
	Timestamp      time.Time `json:"timestamp,omitempty"`
	Request        string    `json:"request,omitempty"`
	ResponseHeader string    `json:"response-header,omitempty"`
	Scheme         string    `json:"scheme,omitempty"`
	Port           string    `json:"port,omitempty"`
	Path           string    `json:"path,omitempty"`
	BodySHA256     string    `json:"body-sha256,omitempty"`
	HeaderSHA256   string    `json:"header-sha256,omitempty"`
	A              []string  `json:"a,omitempty"`
	CNAMEs         []string  `json:"cnames,omitempty"`
	raw            string
	URL            string `json:"url,omitempty"`
	Location       string `json:"location,omitempty"`
	Title          string `json:"title,omitempty"`
	str            string
	err            error
	WebServer      string         `json:"webserver,omitempty"`
	ResponseBody   string         `json:"response-body,omitempty"`
	ContentType    string         `json:"content-type,omitempty"`
	Method         string         `json:"method,omitempty"`
	Host           string         `json:"host,omitempty"`
	ContentLength  int            `json:"content-length,omitempty"`
	StatusCode     int            `json:"status-code,omitempty"`
	TLSData        *httpx.TLSData `json:"tls-grab,omitempty"`
	CSPData        *httpx.CSPData `json:"csp,omitempty"`
	VHost          bool           `json:"vhost,omitempty"`
	WebSocket      bool           `json:"websocket,omitempty"`
	Pipeline       bool           `json:"pipeline,omitempty"`
	HTTP2          bool           `json:"http2,omitempty"`
	CDN            bool           `json:"cdn,omitempty"`
	ResponseTime   string         `json:"response-time,omitempty"`
	Technologies   []string       `json:"technologies,omitempty"`
}

// JSON the result
func (r *Result) JSON() string {
	if js, err := json.Marshal(r); err == nil {
		return string(js)
	}

	return ""
}

func td(s string) string {
	return "<td>" + s + "</td>"
}