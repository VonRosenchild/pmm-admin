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
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"

	"github.com/nu7hatch/gouuid"
	"github.com/percona/platform/proto"
	"gopkg.in/yaml.v2"
)

var VERSION string = "1.0.0"

var (
	ErrNotFound = errors.New("resource not found")
	ErrNoOS     = errors.New("OS not set")
)

type Config struct {
	ClientAddress string `yaml:"client_address"`
	ClientUUID    string `yaml:"client_uuid"`
	ServerAddress string `yaml:"server_address"`
}

type InstanceStatus struct {
	UUID    string
	Type    string
	Name    string // Alias in Prom
	Metrics string // if scraped by Prom
	Queries string // if local agent running QAN
}

type Admin struct {
	filename string
	config   *Config
	api      *API
}

func NewAdmin() *Admin {
	a := &Admin{}
	return a
}

func (a *Admin) LoadConfig(filename string) error {
	if !FileExists(filename) {
		a.filename = filename
		a.config = &Config{}
		return nil
	}
	bytes, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}
	config := &Config{}
	if err := yaml.Unmarshal(bytes, config); err != nil {
		return err
	}
	a.filename = filename
	a.config = config
	return nil
}

func (a *Admin) SetAPI(api *API) {
	a.api = api
}

func (a *Admin) Server() string {
	return a.config.ServerAddress
}

func (a *Admin) SetServer(addr string) error {
	a.config.ServerAddress = addr
	return a.writeConfig()
}

func (a *Admin) ClientAddress() string {
	return a.config.ClientAddress
}

func (a *Admin) OS() (proto.Instance, error) {
	var in proto.Instance

	if a.config.ClientUUID == "" {
		return in, ErrNoOS
	}

	url := a.api.URL(a.config.ServerAddress+":"+proto.DEFAULT_QAN_API_PORT, "instances", a.config.ClientUUID)
	resp, bytes, err := a.api.Get(url)
	if err != nil {
		return in, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return in, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return in, a.api.Error("GET", url, resp.StatusCode, http.StatusOK, bytes)
	}
	if err := json.Unmarshal(bytes, &in); err != nil {
		return in, err
	}

	return in, nil
}

func (a *Admin) AddOS(addr string, start bool) error {
	// Agent creates an OS instance on install. Use its name for the Prom
	// host alias.
	instances, err := a.localAgentInstances()
	if err != nil {
		return err
	}
	if len(instances["os"]) != 1 {
		return fmt.Errorf("agent reported more than 1 OS instance: %+v", instances)
	}
	os := instances["os"][0]

	if start {
		// First start the node_exporter process locally via the metrics API
		// (percona-metrics), else Prom won't have any process to scrape from.
		exp := proto.Exporter{
			Name:  "node_exporter",
			Alias: os.Name + " metrics",
			Port:  "9100",
			Args:  []string{"-collectors.enabled=diskstats,filesystem,loadavg,meminfo,netdev,netstat,stat,time,uname,vmstat"},
		}
		if err := a.startExporter(exp); err != nil {
			return err
		}

		// Add new host to Prom and it will start scraping from this client.
		host := proto.Host{
			Address: addr,
			Alias:   os.Name,
		}
		hostBytes, _ := json.Marshal(host)
		url := a.api.URL(a.config.ServerAddress+":"+proto.DEFAULT_PROM_CONFIG_API_PORT, "hosts", "os")
		resp, content, err := a.api.Post(url, hostBytes)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusCreated {
			return a.api.Error("POST", url, resp.StatusCode, http.StatusCreated, content)
		}
	}

	// Set OS locally.
	a.config.ClientAddress = addr
	a.config.ClientUUID = os.UUID
	return a.writeConfig()
}

func (a *Admin) RemoveOS(name string) error {
	// Remove the host from Prom.
	url := a.api.URL(a.config.ServerAddress+":"+proto.DEFAULT_PROM_CONFIG_API_PORT, "hosts", "os", name)
	resp, content, err := a.api.Delete(url)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// warn?
	default:
		return a.api.Error("DELETE", url, resp.StatusCode, http.StatusOK, content)
	}

	return nil
}

func (a *Admin) AddMySQL(name, dsn, source string, start bool, info map[string]string) error {
	var bytes []byte

	// User must first add the OS which sets the client address.
	if a.config.ClientAddress == "" {
		return ErrNoOS
	}

	// Get OS instance of local agent which is this system. We link new MySQL
	// instance to this OS instance so QAN app knows which agent is handling
	// QAN for this MySQL instance.
	instances, err := a.localAgentInstances()
	if err != nil {
		return err
	}
	if len(instances["os"]) != 1 {
		return fmt.Errorf("agent reported more than 1 OS instance: %+v", instances)
	}

	// Add new MySQL instance to QAN.
	u4, _ := uuid.NewV4()
	uuid := strings.Replace(u4.String(), "-", "", -1)
	in := proto.Instance{
		Subsystem:  "mysql",
		ParentUUID: instances["os"][0].UUID,
		UUID:       uuid,
		Name:       name,
		DSN:        dsn,
		Distro:     info["distro"],
		Version:    info["version"],
	}
	inBytes, _ := json.Marshal(in)
	url := a.api.URL(a.config.ServerAddress+":"+proto.DEFAULT_QAN_API_PORT, "instances")
	resp, content, err := a.api.Post(url, inBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated {
		return a.api.Error("DELETE", url, resp.StatusCode, http.StatusCreated, content)
	}

	// The URI of the new instance is reported in the Location header; fetch it.
	//url = a.api.URL(a.config.ServerAddress+":"+proto.DEFAULT_QAN_API_PORT, resp.Header.Get("Location"))
	url = resp.Header.Get("Location")
	resp, bytes, err = a.api.Get(url)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return a.api.Error("GET", url, resp.StatusCode, http.StatusOK, bytes)
	}
	if err := json.Unmarshal(bytes, &in); err != nil {
		return err
	}

	// Return now if just adding the MySQL instance, else below here we start
	// collecting metrics and queries.
	if !start {
		return nil
	}

	// First start the 3 mysqld_exporter processes locally via the metrics API
	// (percona-metrics), else Prom won't have any process to scrape from. We
	// pass the MySQL instance UUID because the metrics API uses it to fetch
	// the DSN so it can run "DATA_SOURCE_NAME=<DSN> mysqld_exporter ..."
	if err := a.startMySQLExporters(uuid); err != nil {
		return err
	}

	// Add new MySQL host to Prom and it will start scraping from this client.
	host := proto.Host{
		Address: a.config.ClientAddress,
		Alias:   name,
	}
	hostBytes, _ := json.Marshal(host)
	url = a.api.URL(a.config.ServerAddress+":"+proto.DEFAULT_PROM_CONFIG_API_PORT, "hosts", "mysql")
	resp, content, err = a.api.Post(url, hostBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated {
		return a.api.Error("PUT", url, resp.StatusCode, http.StatusCreated, content)
	}

	// Now we have a complete instance resource with ID (UUID), so we can create
	// a QAN config to start that tool. First, we'll need to get the local agent
	// ID to indicate to the QAN API (via the Put() url below) to start QAN on
	// this local agent.
	agentId, err := a.localAgentId()
	if err != nil {
		return err
	}

	// Create a QAN config with no explicitly set vars and agent will use
	// built-in defaults. Then wrap the config in a StartTool cmd.
	qanConfig := map[string]string{
		"UUID":        in.UUID,
		"CollectFrom": source,
	}
	qanConfigBytes, _ := json.Marshal(qanConfig)
	cmd := proto.Cmd{
		User:    "pmm-admin@" + a.api.Hostname(),
		Service: "qan",
		Cmd:     "StartTool",
		Data:    qanConfigBytes,
	}
	cmdBytes, _ := json.Marshal(cmd)

	// Send the StartTool cmd to the API which relays it to the agent, then
	// relays the agent's reply back to here.
	url = a.api.URL(a.config.ServerAddress+":"+proto.DEFAULT_QAN_API_PORT, "agents", agentId, "cmd")
	resp, content, err = a.api.Put(url, cmdBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return a.api.Error("PUT", url, resp.StatusCode, http.StatusOK, content)
	}

	return nil
}

func (a *Admin) RemoveMySQL(name string) error {
	// Remove the host from Prom.
	url := a.api.URL(a.config.ServerAddress+":"+proto.DEFAULT_PROM_CONFIG_API_PORT, "hosts", "mysql", name)
	resp, content, err := a.api.Delete(url)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// warn?
	default:
		return a.api.Error("DELETE", url, resp.StatusCode, http.StatusOK, content)
	}

	// Stop the 3 local mysqld_exporter processes.
	if err := a.stopMySQLExporters(); err != nil {
		return err
	}

	// Get local agent's instances to look up UUID of MySQL instance by name.
	instances, err := a.localAgentInstances()
	if err != nil {
		return err
	}
	var mysqlInstance *proto.Instance
	for _, in := range instances["mysql"] {
		if in.Name != name {
			continue
		}
		mysqlInstance = &in // found it
		break
	}
	if mysqlInstance == nil {
		return nil // not found, warn?
	}

	// Send the StopTool cmd to the API which relays it to the agent, then
	// relays the agent's reply back to here.
	agentId, err := a.localAgentId()
	if err != nil {
		return err
	}

	cmd := proto.Cmd{
		User:    "pmm-admin@" + a.api.Hostname(),
		Service: "qan",
		Cmd:     "StopTool",
		Data:    []byte(mysqlInstance.UUID),
	}
	cmdBytes, _ := json.Marshal(cmd)

	url = a.api.URL(a.config.ServerAddress+":"+proto.DEFAULT_QAN_API_PORT, "agents", agentId, "cmd")
	resp, content, err = a.api.Put(url, cmdBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return a.api.Error("PUT", url, resp.StatusCode, http.StatusOK, content)
	}

	return nil
}

func (a *Admin) List() (map[string][]InstanceStatus, error) {
	status := map[string][]InstanceStatus{
		"os":    []InstanceStatus{},
		"mysql": []InstanceStatus{},
	}

	// Returns {"mysql":[{"Alias":"beatrice.local","Address":"127.0.0.2"}]}
	var hosts map[string][]proto.Host
	url := a.api.URL("localhost:"+proto.DEFAULT_PROM_CONFIG_API_PORT, "hosts")
	resp, bytes, err := a.api.Get(url)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, a.api.Error("GET", url, resp.StatusCode, http.StatusOK, bytes)
	}
	if err := json.Unmarshal(bytes, &hosts); err != nil {
		return nil, err
	}

	// Get local agent configs which contains any QAN configs it's running.
	var configs []proto.AgentConfig
	url = a.api.URL("localhost:"+proto.DEFAULT_AGENT_API_PORT, "configs")
	resp, bytes, err = a.api.Get(url)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, a.api.Error("GET", url, resp.StatusCode, http.StatusOK, bytes)
	}
	if err := json.Unmarshal(bytes, &configs); err != nil {
		return nil, err
	}

	// First, let's get the local OS instance because there should only be one.
	// In Prom, it's the one with the current client address.
	var osHost *proto.Host
	for _, host := range hosts["os"] {
		if host.Address != a.config.ClientAddress {
			continue
		}
		osHost = &host
		break
	}
	if osHost != nil {
		ins := InstanceStatus{
			Type:    "os",
			Name:    osHost.Alias,
			Metrics: "yes",
		}
		status["os"] = append(status["os"], ins)
	}

	// For now we only support 1 MySQL host per instance, i.e. Prom should only
	// be scaping 1 MySQL host from this client.
	var mysqlHost *proto.Host
	for _, host := range hosts["mysql"] {
		if host.Address != a.config.ClientAddress {
			continue
		}
		mysqlHost = &host
		break
	}

	// Get local agent instance to verify that Prom MySQL host = agent QAN host.
	var instances map[string][]proto.Instance
	url = a.api.URL("localhost:"+proto.DEFAULT_AGENT_API_PORT, "instances")
	resp, bytes, err = a.api.Get(url)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, a.api.Error("GET", url, resp.StatusCode, http.StatusOK, bytes)
	}
	if err := json.Unmarshal(bytes, &instances); err != nil {
		return nil, err
	}

	// If Prom and agent have an OS instance with the same name, set its UUID.
	if osHost != nil && len(instances["os"]) > 0 && instances["os"][0].Name == osHost.Alias {
		status["os"][0].UUID = instances["os"][0].UUID
	}

	// Check if the loacl agent is running QAN for the same MySQL host;
	// it should be.
	for _, config := range configs {
		if config.Service != "qan" {
			continue
		}
		for _, in := range instances["mysql"] {
			if in.UUID != config.UUID {
				continue
			}
			// Now we have the QAN config and instance.
			ins := InstanceStatus{
				Type:    "mysql",
				UUID:    in.UUID,
				Name:    in.Name,
				Metrics: "no",
				Queries: "yes",
			}
			if mysqlHost != nil && mysqlHost.Alias == in.Name {
				ins.Metrics = "yes"
				mysqlHost = nil
			}
			status["mysql"] = append(status["mysql"], ins)
		}
	}

	if mysqlHost != nil {
		ins := InstanceStatus{
			Type:    "mysql",
			Name:    osHost.Alias,
			Metrics: "yes",
			Queries: "no",
		}
		status["mysql"] = append(status["mysql"], ins)
	}

	return status, nil
}

func FileExists(file string) bool {
	_, err := os.Stat(file)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	return true
}

// --------------------------------------------------------------------------

func (a *Admin) writeConfig() error {
	bytes, _ := yaml.Marshal(a.config)
	return ioutil.WriteFile(a.filename, bytes, 0644)
}

func (a *Admin) localAgentId() (string, error) {
	url := a.api.URL("localhost:"+proto.DEFAULT_AGENT_API_PORT, "id")
	resp, bytes, err := a.api.Get(url)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", a.api.Error("GET", url, resp.StatusCode, http.StatusOK, bytes)
	}
	return string(bytes), nil
}

func (a *Admin) localAgentInstances() (map[string][]proto.Instance, error) {
	url := a.api.URL("localhost:"+proto.DEFAULT_AGENT_API_PORT, "instances")
	resp, bytes, err := a.api.Get(url)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, a.api.Error("GET", url, resp.StatusCode, http.StatusOK, bytes)
	}
	instances := map[string][]proto.Instance{}
	if err := json.Unmarshal(bytes, &instances); err != nil {
		return nil, err
	}
	return instances, nil
}

func (a *Admin) startMySQLExporters(uuid string) error {
	exp := proto.Exporter{
		Name:         "mysqld_exporter",
		Alias:        "high res",
		Port:         "9104",
		InstanceUUID: uuid,
		Args: []string{
			"-web.listen-address=" + a.config.ClientAddress + ":9104",
			"-collect.global_status=true",
			"-collect.global_variables=false",
			"-collect.slave_status=false",
			"-collect.info_schema.tables=false",
			"-collect.binlog_size=false",
			"-collect.info_schema.processlist=false",
			"-collect.info_schema.userstats=false",
			"-collect.info_schema.tables=false",
			"-collect.auto_increment.columns=false",
			"-collect.info_schema.tablestats=false",
			"-collect.perf_schema.file_events=false",
			"-collect.perf_schema.eventsstatements=false",
			"-collect.perf_schema.indexiowaits=false",
			"-collect.perf_schema.tableiowaits=false",
			"-collect.perf_schema.tablelocks=false",
			"-collect.perf_schema.eventswaits=false",
		},
	}
	if err := a.startExporter(exp); err != nil {
		return err
	}

	exp = proto.Exporter{
		Name:         "mysqld_exporter",
		Alias:        "medium res",
		Port:         "9105",
		InstanceUUID: uuid,
		Args: []string{
			"-web.listen-address=" + a.config.ClientAddress + ":9105",
			"-collect.global_status=false",
			"-collect.global_variables=false",
			"-collect.slave_status=true",
			"-collect.info_schema.tables=false",
			"-collect.binlog_size=false",
			"-collect.info_schema.processlist=true",
			"-collect.info_schema.userstats=false",
			"-collect.info_schema.tables=false",
			"-collect.auto_increment.columns=false",
			"-collect.info_schema.tablestats=false",
			"-collect.perf_schema.file_events=true",
			"-collect.perf_schema.eventsstatements=false",
			"-collect.perf_schema.indexiowaits=false",
			"-collect.perf_schema.tableiowaits=false",
			"-collect.perf_schema.tablelocks=false",
			"-collect.perf_schema.eventswaits=true",
		},
	}
	if err := a.startExporter(exp); err != nil {
		return err
	}

	exp = proto.Exporter{
		Name:         "mysqld_exporter",
		Alias:        "low res",
		Port:         "9106",
		InstanceUUID: uuid,
		Args: []string{
			"-web.listen-address=" + a.config.ClientAddress + ":9106",
			"-collect.global_status=false",
			"-collect.global_variables=true",
			"-collect.slave_status=false",
			"-collect.info_schema.tables=true",
			"-collect.binlog_size=false",
			"-collect.info_schema.processlist=false",
			"-collect.info_schema.userstats=true",
			"-collect.info_schema.tables=true",
			"-collect.auto_increment.columns=true",
			"-collect.info_schema.tablestats=true",
			"-collect.perf_schema.file_events=false",
			"-collect.perf_schema.eventsstatements=true",
			"-collect.perf_schema.indexiowaits=true",
			"-collect.perf_schema.tableiowaits=true",
			"-collect.perf_schema.tablelocks=false",
			"-collect.perf_schema.eventswaits=false",
		},
	}
	if err := a.startExporter(exp); err != nil {
		return err
	}

	return nil
}

func (a *Admin) stopMySQLExporters() error {
	for _, port := range []string{"9104", "9105", "9106"} {
		if err := a.stopExporter("mysqld_exporter", port); err != nil {
			return err
		}
	}
	return nil
}

func (a *Admin) startExporter(exp proto.Exporter) error {
	expBytes, _ := json.Marshal(exp)
	url := a.api.URL("localhost:" + proto.DEFAULT_METRICS_API_PORT)
	resp, content, err := a.api.Post(url, expBytes)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusCreated:
		// success
	case http.StatusConflict:
		if err := a.stopExporter(exp.Name, exp.Port); err != nil {
			return err
		}
		resp, content, err := a.api.Post(url, expBytes) // try again
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusCreated {
			return a.api.Error("(2) POST", url, resp.StatusCode, http.StatusCreated, content)
		}
	default:
		return a.api.Error("(1) POST", url, resp.StatusCode, http.StatusCreated, content)
	}
	return nil // success
}

func (a *Admin) stopExporter(name, port string) error {
	url := a.api.URL("localhost:"+proto.DEFAULT_METRICS_API_PORT, name, port)
	resp, content, err := a.api.Delete(url)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return a.api.Error("DELETE", url, resp.StatusCode, http.StatusOK, content)
	}
	return nil
}
