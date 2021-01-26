package httpx

import (
	"time"
)

// Options contains configuration options for the client
type Options struct {
	DefaultUserAgent string
	RequestOverride  RequestOverride
	HTTPProxy        string
	SocksProxy       string
	Threads          int
	CdnCheck         bool
	// Timeout is the maximum time to wait for the request
	Timeout time.Duration
	// RetryMax is the maximum number of retries
	RetryMax      int
	CustomHeaders map[string]string
	// VHostSimilarityRatio 1 - 100
	VHostSimilarityRatio int
	FollowRedirects      bool
	FollowHostRedirects  bool
	Unsafe               bool
	// VHOSTs options
	VHostIgnoreStatusCode    bool
	VHostIgnoreContentLength bool
	VHostIgnoreNumberOfWords bool
	VHostIgnoreNumberOfLines bool
	VHostStripHTML           bool
}

// DefaultOptions contains the default options
var DefaultOptions = Options{
	Threads:  25,
	Timeout:  30 * time.Second,
	RetryMax: 5,
	Unsafe:   false,
	CdnCheck: true,
	// VHOSTs options
	VHostIgnoreStatusCode:    false,
	VHostIgnoreContentLength: true,
	VHostIgnoreNumberOfWords: false,
	VHostIgnoreNumberOfLines: false,
	VHostStripHTML:           false,
	VHostSimilarityRatio:     85,
	DefaultUserAgent:         "User-Agent: Mozilla/5.0(Linux;U;Android6.0.1;zh-cn;GT-S5660Build/GINGERBREAD)AppleWebKit/533.1(KHTML,likeGecko)Version/4.0MobileSafari/533.1MicroMessenger/4.5.255",
}
