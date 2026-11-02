package main

// Lookups through hdl.handle.net's REST API: the global registry's record for
// a prefix, which says which servers answer for it, and the resolution that
// users get when they follow a handle link.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type jsonRecord struct {
	ResponseCode int    `json:"responseCode"`
	Message      string `json:"message"`
	Values       []struct {
		Type string `json:"type"`
		Data struct {
			Value json.RawMessage `json:"value"`
		} `json:"data"`
	} `json:"values"`
}

// lookup fetches a handle's record from hdl.handle.net. A handle that doesn't
// exist isn't an error; check the record's response code.
func lookup(handle string, timeout time.Duration) (*jsonRecord, error) {
	// "auth" skips the proxy's cache and asks the handle's primary site
	u := url.URL{Scheme: "https", Host: "hdl.handle.net", Path: "/api/handles/" + handle, RawQuery: "auth=true"}
	resp, err := (&http.Client{Timeout: timeout}).Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// The API answers "not found" and the like with an error status and a
	// JSON body
	var rec jsonRecord
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMessage)).Decode(&rec); err != nil {
		return nil, fmt.Errorf("%s: HTTP status %s", u.Redacted(), resp.Status)
	}
	if rec.ResponseCode != rcSuccess && rec.ResponseCode != rcHandleNotFound {
		return nil, fmt.Errorf("looking up %s: %w", handle, &responseError{code: uint32(rec.ResponseCode), message: rec.Message})
	}
	return &rec, nil
}

// registeredSites returns the sites the global registry has for prefix, or
// nil if the prefix isn't registered. The prefix's record (0.NA/<prefix>)
// lists its sites in HS_SITE values, or names another record that does in an
// HS_SERV value.
func registeredSites(prefix string, timeout time.Duration) ([]*siteInfo, error) {
	handle := "0.NA/" + prefix
	for range 2 {
		rec, err := lookup(handle, timeout)
		if err != nil {
			return nil, err
		}
		if rec.ResponseCode == rcHandleNotFound {
			return nil, nil
		}

		var sites []*siteInfo
		var serv string
		for _, v := range rec.Values {
			switch v.Type {
			case "HS_SITE":
				site, err := siteFromJSON(v.Data.Value)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", handle, err)
				}
				sites = append(sites, site)
			case "HS_SERV":
				json.Unmarshal(v.Data.Value, &serv)
			}
		}
		if len(sites) > 0 {
			return sites, nil
		}
		if serv == "" {
			return nil, fmt.Errorf("%s has no HS_SITE or HS_SERV values", handle)
		}
		handle = serv
	}
	return nil, errors.New("too many HS_SERV redirections")
}

// globalURLs resolves a handle through hdl.handle.net and returns its URLs
func globalURLs(handle string, timeout time.Duration) ([]string, error) {
	rec, err := lookup(handle, timeout)
	if err != nil {
		return nil, err
	}
	if rec.ResponseCode != rcSuccess {
		return nil, &responseError{code: uint32(rec.ResponseCode), message: rec.Message}
	}
	var urls []string
	for _, v := range rec.Values {
		var s string
		if v.Type == "URL" && json.Unmarshal(v.Data.Value, &s) == nil {
			urls = append(urls, s)
		}
	}
	return urls, nil
}
