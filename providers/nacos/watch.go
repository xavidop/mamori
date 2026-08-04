package nacos

import (
	"context"
	"crypto/md5" //nolint:gosec // Nacos's listener protocol names MD5; see contentMD5.
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// listenerPath is Nacos's long-poll listener endpoint, relative to the servlet
// context path.
const listenerPath = "v1/cs/configs/listener"

// listeningConfigsParam is the form field the listener endpoint reads the probe
// from. The capitalisation is Nacos's, and it is a POST body parameter despite
// looking like a header name.
const listeningConfigsParam = "Listening-Configs"

// longPullingTimeoutHeader tells the listener how long to hold the request open,
// in milliseconds.
//
// "Pulling" is Nacos's own spelling of "polling" and it is not a typo in this
// file: the constant in the Nacos server is
// LONG_POLLING_HEADER = "Long-Pulling-Timeout" (LongPollingService.java), and
// isSupportLongPolling decides whether to park a request purely on whether that
// header is present.
//
// Correcting the spelling does not break the watch, which is what makes it
// dangerous. Nacos falls back to SHORT polling for a request without the header:
// it compares the MD5s and answers immediately either way. The watch would still
// report every change, so every behavioural test would still pass, while every
// round returned instantly and the loop hammered the server as fast as
// httpcore.LongPoll would let it. Only an assertion on the request itself
// catches that, which is why one exists.
const longPullingTimeoutHeader = "Long-Pulling-Timeout"

// wordSeparator separates the fields WITHIN one configuration probe: dataId,
// group, contentMD5, and tenant.
//
// It is ASCII 0x02 (STX), an invisible control character, which the Nacos Open
// API documents as "^2 = Character.toString((char) 2), The url encoded value is
// %02". It reaches the wire as %02 because the probe travels as an ordinary
// application/x-www-form-urlencoded form value.
const wordSeparator = "\x02"

// lineSeparator terminates one configuration probe, so several can be sent in
// one request.
//
// It is ASCII 0x01 (SOH), documented as "^1 = Character.toString((char) 1), The
// url encoded value is %01". The trailing separator is required, not optional:
// the server splits on it and a probe without one is not a complete record.
const lineSeparator = "\x01"

// Watch implements mamori.WatchableProvider on Nacos's long-poll listener.
//
// Nacos is the one backend in this repository whose change notification is
// neither a stream nor a blocking read of the value, but a comparison: a round
// POSTs "here is the MD5 I believe this configuration currently has" and the
// server holds the request open until either that belief becomes wrong or the
// hold elapses. Nothing is pushed, and the response names only WHICH
// configuration moved, never its content, so a round that reports a change
// follows it with an ordinary read.
//
// That shape is what makes the watch gap-free without any coordination, and it
// is why this provider can honestly declare
// providertest.Config.WatchDeliversBaseline. The baseline read and the MD5 the
// first round subscribes with come from the same response, so a write landing
// between them cannot be missed: the round simply carries an MD5 that is already
// stale, and the server answers it immediately rather than parking it. A
// stream-based watch has to establish its subscription before it reads, because
// its notifications only cover what happens after it attaches; a comparison-based
// one carries the "before" with it in every request.
//
// The loop itself - one goroutine, one round at a time, a client deadline longer
// than the hold the server was given, closure on context cancellation, and no
// re-attempt of a round already reported - is httpcore.LongPoll. This function
// supplies only the two Nacos-specific halves: what a baseline is and what a
// round sends.
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	c, err := p.coordinatesFor(ref)
	if err != nil {
		return nil, err
	}
	if _, err := p.core(); err != nil {
		// Fail before the goroutine starts, so a misconfigured provider falls
		// back to mamori's polling adapter (see watchRef) instead of producing
		// a live channel that only ever carries the same construction error.
		return nil, err
	}

	// known is the MD5 this watch currently believes the configuration has, and
	// it is the entire subscription state. It needs no lock: httpcore.LongPoll
	// runs Baseline and every Round on one goroutine, one at a time.
	var known string

	// observe performs the read that follows a baseline or a reported change,
	// updates known, and turns the body into a result.
	//
	// A configuration that does not exist is NOT an error here. It sets known to
	// the empty MD5, which is exactly what Nacos's own client sends for a
	// configuration it does not hold, so the next round is a live subscription
	// to the configuration being CREATED. Emitting nothing for it matches
	// mamori's polling adapter, which is silent for an absent key, and keeps a
	// native watch and a polled one indistinguishable to the reconciler.
	observe := func(ctx context.Context) (httpcore.LongPollResult, error) {
		body, err := p.fetch(ctx, c)
		if errors.Is(err, mamori.ErrNotFound) {
			known = ""
			return httpcore.LongPollResult{}, nil
		}
		if err != nil {
			return httpcore.LongPollResult{}, err
		}
		known = contentMD5(body)
		val, err := p.value(body, ref)
		if err != nil {
			return httpcore.LongPollResult{}, err
		}
		return httpcore.LongPollResult{Changed: true, Value: val}, nil
	}

	return httpcore.LongPoll(ctx, httpcore.LongPollConfig{
		Hold:     p.hold,
		Baseline: observe,
		Round: func(ctx context.Context, hold time.Duration) (httpcore.LongPollResult, error) {
			changed, err := p.listen(ctx, c, known, hold)
			if err != nil || !changed {
				return httpcore.LongPollResult{}, err
			}
			return observe(ctx)
		},
	})
}

// listen performs one long-poll round and reports whether the configuration
// moved.
//
// The probe is
//
//	dataId \x02 group \x02 contentMD5 \x02 tenant \x01
//
// with the tenant field omitted (and its preceding separator with it) when the
// provider has no namespace, which is the form Nacos's docs give as the
// alternative: "dataId^2group^2contentMD5^1". It is sent as the value of the
// Listening-Configs form field, so url.Values.Encode percent-encodes the two
// control characters into the %02 and %01 the endpoint expects.
func (p *Provider) listen(ctx context.Context, c coordinates, md5sum string, hold time.Duration) (bool, error) {
	client, err := p.core()
	if err != nil {
		return false, err
	}

	fields := []string{c.dataID, c.group, md5sum}
	if t := p.tenant(); t != "" {
		fields = append(fields, t)
	}
	probe := strings.Join(fields, wordSeparator) + lineSeparator

	form := url.Values{listeningConfigsParam: {probe}}
	resp, err := client.Do(ctx, httpcore.Request{
		Method: http.MethodPost,
		Path:   listenerPath,
		Header: http.Header{
			"Content-Type": {"application/x-www-form-urlencoded"},
			// Milliseconds, as the Open API states: "The timeout for long
			// polling is 30s. Enter 30,000 here."
			longPullingTimeoutHeader: {strconv.FormatInt(hold.Milliseconds(), 10)},
		},
		Body: []byte(form.Encode()),
	})
	if err != nil {
		return false, fmt.Errorf("nacos: listen %s: %w", c, err)
	}
	return changedConfigs(resp.Body, c, p.tenant()), nil
}

// changedConfigs reports whether the listener's response names c.
//
// The response is the part of this protocol most easily got wrong, so it is
// worth stating exactly what arrives. Nacos builds it in
// MD5Util.compareMd5ResultString, which assembles
//
//	dataId \x02 group \x02 tenant \x01
//
// and then returns URLEncoder.encode(that, "UTF-8"). The body on the wire
// therefore contains the six literal ASCII characters "%02" and "%01", not the
// control bytes themselves. Splitting the raw body on "\x01" finds nothing, so
// the watch reports "unchanged" forever - it never errors, never logs, and
// never fires. That is the single worst failure this file can have, and it is
// why the decode happens first and why an explicit test asserts a change is
// detected from the URL-encoded form.
//
// A body that does not decode is used as-is rather than rejected. The encoded
// form is what the reference server sends, but the endpoint sits behind
// whatever proxy an operator put in front of it, and tolerating the raw form
// costs one fallback while refusing it would mean a silently dead watch again.
//
// The entry is matched against the coordinates this round asked about rather
// than treating any non-empty body as a change. Only one configuration is ever
// registered per round, so a body naming something else means the request did
// not go where it was meant to, and treating that as a change would send this
// provider into a read-and-emit loop against a configuration nobody is watching.
func changedConfigs(body []byte, c coordinates, tenant string) bool {
	raw := strings.TrimSpace(string(body))
	if raw == "" {
		// The hold elapsed with nothing to report. This is the ordinary
		// outcome, several times an hour, for every watched configuration.
		return false
	}
	decoded, err := url.QueryUnescape(raw)
	if err != nil {
		decoded = raw
	}

	for entry := range strings.SplitSeq(decoded, lineSeparator) {
		if entry == "" {
			continue
		}
		fields := strings.Split(entry, wordSeparator)
		if len(fields) < 2 {
			continue
		}
		if fields[0] != c.dataID || fields[1] != c.group {
			continue
		}
		// The tenant field is present only when the request carried one, and
		// Nacos omits it when it is blank even then, so it is checked only when
		// both sides have one to compare.
		if len(fields) > 2 && tenant != "" && fields[2] != tenant {
			continue
		}
		return true
	}
	return false
}

// contentMD5 is the hex MD5 digest of a configuration's content, which is the
// only identity Nacos's listener protocol compares.
//
// MD5 is not a security choice here and there is no stronger one available: the
// server computes MD5 over the stored content and compares it byte-for-byte with
// what a client sends, so any other digest makes every configuration look
// permanently changed. An empty configuration hashes to the empty string rather
// than to MD5(""), matching what the Nacos client sends for content it does not
// hold, so that a probe for an absent configuration is a subscription to its
// creation rather than a claim about its contents.
func contentMD5(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	sum := md5.Sum(body) //nolint:gosec // protocol-mandated digest, not a security primitive
	return hex.EncodeToString(sum[:])
}

// String renders coordinates for an error message. It carries no value, only the
// address of one.
func (c coordinates) String() string {
	return "dataId=" + c.dataID + " group=" + c.group
}
