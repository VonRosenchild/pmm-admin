/*
   Copyright (c) 2016, Percona LLC and/or its affiliates. All rights reserved.

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, either version 3 of the License, or
   (at your option) any later version.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>
*/

package pmm

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	AGENT_API_PORT string = "9000"
	QAN_API_PORT   string = "9001"
	PROM_API_PORT  string = "9003"
)

type API struct {
	headers  map[string]string
	hostname string
}

func NewAPI(headers map[string]string) *API {
	hostname, _ := os.Hostname()
	a := &API{
		headers:  headers,
		hostname: hostname,
	}
	return a
}

func (a *API) Hostname() string {
	return a.hostname
}

func (a *API) Ping(url string) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	if a.headers != nil {
		for k, v := range a.headers {
			req.Header.Add(k, v)
		}
	}

	client := newClient()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, err = ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("got status code %d, expected 200", resp.StatusCode)
	}
	return nil // success
}

func (a *API) URL(addr string, paths ...string) string {
	schema := "http://"
	httpPrefix := "http://"
	if strings.HasPrefix(addr, httpPrefix) {
		addr = strings.TrimPrefix(addr, httpPrefix)
	}
	//if strings.HasPrefix(addr, "localhost") || strings.HasPrefix(addr, "127.0.0.1") {
	//	schema = httpPrefix
	//}
	slash := "/"
	if len(paths) > 0 && paths[0][0] == 0x2F {
		slash = ""
	}
	return schema + addr + slash + strings.Join(paths, "/")
}

func (a *API) Get(url string) (*http.Response, []byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, nil, err
	}
	if a.headers != nil {
		for k, v := range a.headers {
			req.Header.Add(k, v)
		}
	}

	client := newClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	var data []byte
	if resp.Header.Get("Content-Type") == "application/x-gzip" {
		buf := new(bytes.Buffer)
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("GET %s: gzip.NewReader: %s", url, err)
		}
		if _, err := io.Copy(buf, gz); err != nil {
			return resp, nil, fmt.Errorf("GET %s: io.Copy: %s", url, err)
		}
		data = buf.Bytes()
	} else {
		data, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			return resp, nil, fmt.Errorf("GET %s: ioutil.ReadAll: %s", url, err)
		}
	}

	return resp, data, nil
}

func (a *API) Post(url string, data []byte) (*http.Response, []byte, error) {
	return a.send("POST", url, data)
}

func (a *API) Put(url string, data []byte) (*http.Response, []byte, error) {
	return a.send("PUT", url, data)
}

func (a *API) Delete(url string) (*http.Response, []byte, error) {
	return a.send("DELETE", url, nil)
}

// --------------------------------------------------------------------------

func newClient() *http.Client {
	return &http.Client{Timeout: time.Duration(5 * time.Second)}
}

func (a *API) send(method, url string, data []byte) (*http.Response, []byte, error) {
	var req *http.Request
	var err error
	if data != nil {
		req, err = http.NewRequest(method, url, bytes.NewReader(data))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		return nil, nil, err
	}
	if a.headers != nil {
		for k, v := range a.headers {
			req.Header.Add(k, v)
		}
	}

	client := newClient()
	resp, err := client.Do(req)
	if err != nil {
		return resp, nil, err
	}

	content, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return resp, nil, err
	}

	return resp, content, nil
}
