package crawler

import (
	"testing"

	"github.com/chromedp/cdproto/network"
)

// TestCollectorReqCountSkipsRedirects verifies the request counter counts a
// redirected resource ONCE: EventRequestWillBeSent fires again per 3xx hop on the
// same RequestID (carrying RedirectResponse) and those continuations must not
// increment the count. weightBytes sums EventLoadingFinished (once per request).
func TestCollectorReqCountSkipsRedirects(t *testing.T) {
	c := &collector{
		reqURLs:   map[network.RequestID]string{},
		recvBytes: map[network.RequestID]int64{},
	}

	// Initial request for resource A (no RedirectResponse).
	c.handle(&network.EventRequestWillBeSent{RequestID: "A", Request: &network.Request{URL: "http://x.test/a"}})
	// Two redirect hops on the SAME request id → must NOT count.
	c.handle(&network.EventRequestWillBeSent{RequestID: "A", RedirectResponse: &network.Response{URL: "http://x.test/a1"}, Request: &network.Request{URL: "http://x.test/a2"}})
	c.handle(&network.EventRequestWillBeSent{RequestID: "A", RedirectResponse: &network.Response{URL: "http://x.test/a2"}, Request: &network.Request{URL: "http://x.test/final"}})
	// A second, independent request.
	c.handle(&network.EventRequestWillBeSent{RequestID: "B", Request: &network.Request{URL: "http://x.test/b"}})

	if c.reqCount != 2 {
		t.Errorf("reqCount = %d, want 2 (one redirected + one plain; redirect hops not counted)", c.reqCount)
	}

	// Loading finished sums encoded bytes per request.
	c.handle(&network.EventLoadingFinished{RequestID: "A", EncodedDataLength: 1000})
	c.handle(&network.EventLoadingFinished{RequestID: "B", EncodedDataLength: 500})
	if c.weightBytes != 1500 {
		t.Errorf("weightBytes = %d, want 1500", c.weightBytes)
	}
}

// TestCollectorWeightMainDocumentFallback verifies the two page-weight accounting
// paths: subresources carry an authoritative non-zero loadingFinished total, while
// the MAIN navigation document reports loadingFinished.encodedDataLength=0 and its
// real transferred bytes arrive only as Network.dataReceived chunks. The collector
// must count the document's summed dataReceived bytes as a FALLBACK when the
// authoritative total is 0, and must NOT double-count when the total is present.
func TestCollectorWeightMainDocumentFallback(t *testing.T) {
	c := &collector{
		reqURLs:   map[network.RequestID]string{},
		recvBytes: map[network.RequestID]int64{},
	}

	// Main document: dataReceived chunks (300 + 200) then loadingFinished(0).
	// The summed 500 must be the fallback contribution.
	c.handle(&network.EventRequestWillBeSent{RequestID: "DOC", Request: &network.Request{URL: "http://x.test/"}})
	c.handle(&network.EventDataReceived{RequestID: "DOC", EncodedDataLength: 300})
	c.handle(&network.EventDataReceived{RequestID: "DOC", EncodedDataLength: 200})
	c.handle(&network.EventLoadingFinished{RequestID: "DOC", EncodedDataLength: 0})

	if c.weightBytes != 500 {
		t.Fatalf("weightBytes = %d, want 500 (main-document dataReceived fallback: 300+200)", c.weightBytes)
	}

	// Subresource: a dataReceived chunk (300) plus an authoritative
	// loadingFinished(500). The authoritative total wins — the contribution must be
	// 500, NOT 800 (no double count of the dataReceived bytes).
	c.handle(&network.EventRequestWillBeSent{RequestID: "SUB", Request: &network.Request{URL: "http://x.test/app.js"}})
	c.handle(&network.EventDataReceived{RequestID: "SUB", EncodedDataLength: 300})
	c.handle(&network.EventLoadingFinished{RequestID: "SUB", EncodedDataLength: 500})

	if c.weightBytes != 1000 {
		t.Fatalf("weightBytes = %d, want 1000 (500 doc + 500 subresource; authoritative total wins, no double count)", c.weightBytes)
	}

	// The per-request accumulator is cleared on loadingFinished (no leak).
	if len(c.recvBytes) != 0 {
		t.Errorf("recvBytes not cleared: %v", c.recvBytes)
	}
}
