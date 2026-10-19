package config

import (
	"fmt"
	"net/url"
)

func sessionQueryURL(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("PROXY_SESSION_QUERY_URL must be an http(s) service URL without credentials, query or fragment")
	}
	return u, nil
}
