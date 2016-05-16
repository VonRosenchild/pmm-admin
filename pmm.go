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

	"github.com/percona/pmm/proto"
	"gopkg.in/yaml.v2"
)

const (
	VERSION     = "1.0.1"
	CONSUL_PORT = "8500"
)

var (
	ErrNotFound     = errors.New("resource not found")
	ErrNoOS         = errors.New("OS not set")
	ErrHostConflict = errors.New("host conflict")
)

type Config struct {
	ClientAddress string `yaml:"client_address"`
	ClientUUID    string `yaml:"client_uuid"`
	ServerAddress string `yaml:"server_address"`
}

type ConsulService struct {
	Service string
	Port    uint16
	Tags    []string `json:"Tags,omitempty"`
}

type ConsulNode struct {
	Node      string
	Address   string
	Service   ConsulService
	ServiceID string
}

type InstanceStatus struct {
	Type string
	Name string
	Tags interface{}
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

func (a *Admin) AddOS(addr string, start bool, replset string, cluster string) error {
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

	// Check if Consul is already monitoring this OS.
	ok, err := a.serviceExists(os.Name, "linux")
	if err != nil {
		return err
	}
	if ok {
		return fmt.Errorf("PMM is already monitoring this OS instance %s", os.Name)
	}

	if start {
		// Start node_exporter via process manager API.
		exp := proto.Exporter{
			Name:  "node_exporter",
			Alias: "System metrics",
			Port:  "9100",
			Args: []string{fmt.Sprintf("-web.listen-address=%s:9100", addr),
				"-collectors.enabled=diskstats,filesystem,loadavg,meminfo,netdev,netstat,stat,time,uname,vmstat"},
		}
		if err := a.startExporter(exp); err != nil {
			return err
		}

		// Add linux service to Consul.
		var tags []string
		if replset != "" {
			tags = append(tags, fmt.Sprintf("replset_%s", replset))
		}
		if cluster != "" {
			tags = append(tags, fmt.Sprintf("cluster_%s", cluster))
		}

		host := ConsulNode{
			Node:    os.Name,
			Address: addr,
			Service: ConsulService{Service: "linux", Port: 9100, Tags: tags},
		}
		hostBytes, _ := json.Marshal(host)
		url := a.api.URL(a.config.ServerAddress+":"+CONSUL_PORT, "v1", "catalog", "register")
		resp, content, err := a.api.Put(url, hostBytes)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return a.api.Error("PUT", url, resp.StatusCode, http.StatusOK, content)
		}
	}

	// Set OS locally.
	a.config.ClientAddress = addr
	a.config.ClientUUID = os.UUID
	return a.writeConfig()
}

func (a *Admin) RemoveOS(name string) error {
	// Check if Consul is already monitoring this OS.
	ok, err := a.serviceExists(name, "linux")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("PMM is not monitoring this OS instance")
	}

	// Remove service from Consul.
	host := ConsulNode{
		Node:      name,
		ServiceID: "linux",
	}
	hostBytes, _ := json.Marshal(host)
	url := a.api.URL(a.config.ServerAddress+":"+CONSUL_PORT, "v1", "catalog", "deregister")
	resp, content, err := a.api.Put(url, hostBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return a.api.Error("PUT", url, resp.StatusCode, http.StatusOK, content)
	}

	// Stop node_exporter process.
	if err := a.stopExporter("node_exporter", "9100"); err != nil {
		return err
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
	in := proto.Instance{
		Subsystem:  "mysql",
		ParentUUID: instances["os"][0].UUID,
		Name:       name, // unique ID
		// Do not set UUID here, let API do it because if we get a StatusConflict
		// below, we want the existing instance UUID
		DSN:     dsn,
		Distro:  info["distro"],
		Version: info["version"],
	}
	inBytes, _ := json.Marshal(in)
	url := a.api.URL(a.config.ServerAddress+":"+proto.DEFAULT_QAN_API_PORT, "instances")
	resp, content, err := a.api.Post(url, inBytes)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusCreated:
	case http.StatusConflict:
		// instance already exists based on Name
	default:
		return a.api.Error("POST", url, resp.StatusCode, http.StatusCreated, content)
	}

	// The URI of the new instance is reported in the Location header; fetch it.
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

	uuid := in.UUID

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

	// Check if Consul is already monitoring this MySQL.
	ok, err := a.serviceExists(name, "mysql-hr")
	if err != nil {
		return err
	}
	if ok {
		return fmt.Errorf("PMM is already monitoring this MySQL instance %s", name)
	}

	// Add 3 mysql services to Consul.
	for job, port := range map[string]uint16{"mysql-hr": 9104, "mysql-mr": 9105, "mysql-lr": 9106} {
		host := ConsulNode{
			Node:    name,
			Address: a.config.ClientAddress,
			Service: ConsulService{Service: job, Port: port},
		}
		hostBytes, _ := json.Marshal(host)
		url = a.api.URL(a.config.ServerAddress+":"+CONSUL_PORT, "v1", "catalog", "register")
		resp, content, err = a.api.Put(url, hostBytes)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return a.api.Error("PUT", url, resp.StatusCode, http.StatusOK, content)
		}
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
	a.stopQAN(agentId, in)
	qanConfig := map[string]string{
		"UUID":        in.UUID,
		"CollectFrom": source,
	}
	if err := a.startQAN(agentId, in, qanConfig); err != nil {
		return err
	}

	return nil
}

func (a *Admin) RemoveMySQL(name string) error {
	// Check if Consul is already monitoring this OS.
	ok, err := a.serviceExists(name, "mysql-hr")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("PMM is not monitoring this MySQL instance")
	}

	// Remove 3 mysql services from Consul.
	for _, job := range []string{"mysql-hr", "mysql-mr", "mysql-lr"} {
		host := ConsulNode{
			Node:      name,
			ServiceID: job,
		}
		hostBytes, _ := json.Marshal(host)
		url := a.api.URL(a.config.ServerAddress+":"+CONSUL_PORT, "v1", "catalog", "deregister")
		resp, content, err := a.api.Put(url, hostBytes)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return a.api.Error("PUT", url, resp.StatusCode, http.StatusOK, content)
		}
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
		fmt.Printf("percona-qan-agent is not monitoring MySQL instance %s\n", name)
		return nil
	}

	// Stop QAN for this MySQL instance on the local agent.
	agentId, err := a.localAgentId()
	if err != nil {
		return err
	}
	if err := a.stopQAN(agentId, *mysqlInstance); err != nil {
		return err
	}

	return nil
}

func (a *Admin) AddMongoDB(name string, start bool, uri string, replset string, cluster string) error {
	// User must first add the OS which sets the client address.
	if a.config.ClientAddress == "" {
		return ErrNoOS
	}
	if !start {
		return nil
	}

	// Agent creates an OS instance on install. Use its name for the Prom host alias.
	instances, err := a.localAgentInstances()
	os := instances["os"][0]

	// Check if Consul is already monitoring this MongoDB.
	ok, err := a.serviceExists(os.Name, "mongodb")
	if err != nil {
		return err
	}
	if ok {
		return fmt.Errorf("PMM is already monitoring this MongoDB instance %s", os.Name)
	}

	// Start mongodb_exporter via process manager API.
	args := []string{fmt.Sprintf("-web.listen-address=%s:9107", a.config.ClientAddress)}
	if uri != "" {
		args = append(args, fmt.Sprintf("-mongodb.uri=%s", uri))
	}
	exp := proto.Exporter{
		Name:  "mongodb_exporter",
		Alias: "MongoDB metrics",
		Port:  "9107",
		Args:  args,
	}
	if err := a.startExporter(exp); err != nil {
		return err
	}

	// Add mongodb service to Consul.
	var tags []string
	if replset != "" {
		tags = append(tags, fmt.Sprintf("replset_%s", replset))
	}
	if cluster != "" {
		tags = append(tags, fmt.Sprintf("cluster_%s", cluster))
	}

	host := ConsulNode{
		Node:    os.Name,
		Address: a.config.ClientAddress,
		Service: ConsulService{Service: "mongodb", Port: 9107, Tags: tags},
	}
	hostBytes, _ := json.Marshal(host)
	url := a.api.URL(a.config.ServerAddress+":"+CONSUL_PORT, "v1", "catalog", "register")
	resp, content, err := a.api.Put(url, hostBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return a.api.Error("PUT", url, resp.StatusCode, http.StatusOK, content)
	}

	return nil
}

func (a *Admin) RemoveMongoDB(name string) error {
	// Check if Consul is already monitoring this OS.
	ok, err := a.serviceExists(name, "mongodb")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("PMM is not monitoring this MongoDB instance")
	}

	// Remove service from Consul.
	host := ConsulNode{
		Node:      name,
		ServiceID: "mongodb",
	}
	hostBytes, _ := json.Marshal(host)
	url := a.api.URL(a.config.ServerAddress+":"+CONSUL_PORT, "v1", "catalog", "deregister")
	resp, content, err := a.api.Put(url, hostBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return a.api.Error("PUT", url, resp.StatusCode, http.StatusOK, content)
	}

	// Stop mongodb_exporter process.
	if err := a.stopExporter("mongodb_exporter", "9107"); err != nil {
		return err
	}

	return nil
}

func (a *Admin) List() (map[string][]InstanceStatus, error) {
	// User must first add the OS which sets the client address.
	if a.config.ClientAddress == "" {
		return nil, ErrNoOS
	}

	// Agent creates an OS instance on install. Use its name for the Prom host alias.
	agent_instances, err := a.localAgentInstances()
	os := agent_instances["os"][0]

	status := map[string][]InstanceStatus{
		"os":      []InstanceStatus{},
		"mysql":   []InstanceStatus{},
		"mongodb": []InstanceStatus{},
	}

	// curl http://192.168.56.107:8500/v1/catalog/node/centos7.vm
	url := a.api.URL(a.config.ServerAddress+":"+CONSUL_PORT, "v1", "catalog", "node", os.Name)
	resp, bytes, err := a.api.Get(url)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, a.api.Error("GET", url, resp.StatusCode, http.StatusOK, bytes)
	}

	var data interface{}
	if string(bytes) == "null" {
		// Node does not exist
		return nil, nil
	} else if err = json.Unmarshal(bytes, &data); err != nil {
		// Node exists
		return nil, err
	}

	// Check services
	for action, job := range map[string]string{"os": "linux", "mysql": "mysql-hr", "mongodb": "mongodb"} {
		if services, ok := data.(map[string]interface{})["Services"]; ok {
			if srv, ok := services.(map[string]interface{})[job]; ok {
				ins := InstanceStatus{
					Type: job,
					Name: os.Name,
					Tags: srv.(map[string]interface{})["Tags"],
				}
				status[action] = append(status[action], ins)
			}
		}
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
	//if len(instances["os"]) > 0 && instances["os"][0].Name == os.Name {
	//	status["os"][0].UUID = instances["os"][0].UUID
	//}

	// Check if the local agent is running QAN for the same MySQL host;
	// it should be.
	//var mysqlHost interface{}
	//for _, config := range configs {
	//	if config.Service != "qan" {
	//		continue
	//	}
	//	for _, in := range instances["mysql"] {
	//		if in.UUID != config.UUID {
	//			continue
	//		}
	//		// Now we have the QAN config and instance.
	//		ins := InstanceStatus{
	//			Type:    "mysql",
	//			UUID:    in.UUID,
	//			Name:    in.Name,
	//			Metrics: "no",
	//			Queries: "yes",
	//		}
	//		if mysqlHost != nil && os.Name == in.Name {
	//			ins.Metrics = "yes"
	//			mysqlHost = nil
	//		}
	//		status["mysql"] = append(status["mysql"], ins)
	//	}
	//}
	//
	//if mysqlHost != nil {
	//	ins := InstanceStatus{
	//		Type:    "mysql",
	//		Name:    os.Name,
	//		Metrics: "yes",
	//		Queries: "no",
	//	}
	//	status["mysql"] = append(status["mysql"], ins)
	//}

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
		Alias:        "MySQL high-res metrics",
		Port:         "9104",
		InstanceUUID: uuid,
		Args: []string{
			"-web.listen-address=" + a.config.ClientAddress + ":9104",
			"-collect.global_status=true",
			"-collect.global_variables=false",
			"-collect.slave_status=false",
			"-collect.info_schema.tables=false",
			"-collect.binlog_size=false",
			"-collect.engine_tokudb_status=false",
			"-collect.info_schema.innodb_metrics=true",
			"-collect.info_schema.processlist=false",
			"-collect.info_schema.userstats=false",
			"-collect.info_schema.query_response_time=false",
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
		Alias:        "MySQL medium-res metrics",
		Port:         "9105",
		InstanceUUID: uuid,
		Args: []string{
			"-web.listen-address=" + a.config.ClientAddress + ":9105",
			"-collect.global_status=false",
			"-collect.global_variables=false",
			"-collect.slave_status=true",
			"-collect.info_schema.tables=false",
			"-collect.binlog_size=false",
			"-collect.engine_tokudb_status=false",
			"-collect.info_schema.innodb_metrics=false",
			"-collect.info_schema.processlist=true",
			"-collect.info_schema.userstats=false",
			"-collect.info_schema.query_response_time=true",
			"-collect.auto_increment.columns=false",
			"-collect.info_schema.tablestats=false",
			"-collect.perf_schema.file_events=true",
			"-collect.perf_schema.eventsstatements=false",
			"-collect.perf_schema.indexiowaits=false",
			"-collect.perf_schema.tableiowaits=false",
			"-collect.perf_schema.tablelocks=true",
			"-collect.perf_schema.eventswaits=true",
		},
	}
	if err := a.startExporter(exp); err != nil {
		return err
	}

	exp = proto.Exporter{
		Name:         "mysqld_exporter",
		Alias:        "MySQL low-res metrics",
		Port:         "9106",
		InstanceUUID: uuid,
		Args: []string{
			"-web.listen-address=" + a.config.ClientAddress + ":9106",
			"-collect.global_status=false",
			"-collect.global_variables=true",
			"-collect.slave_status=false",
			"-collect.info_schema.tables=true",
			"-collect.binlog_size=true",
			"-collect.engine_tokudb_status=false",
			"-collect.info_schema.innodb_metrics=false",
			"-collect.info_schema.processlist=false",
			"-collect.info_schema.userstats=true",
			"-collect.info_schema.query_response_time=false",
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
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		fmt.Printf("percona-prom-pm is not running %s:%s\n", name, port)
	default:
		return a.api.Error("DELETE", url, resp.StatusCode, http.StatusOK, content)
	}
	return nil
}

func (a *Admin) startQAN(agentId string, in proto.Instance, config map[string]string) error {
	configBytes, _ := json.Marshal(config)
	cmd := proto.Cmd{
		User:    "pmm-admin@" + a.api.Hostname(),
		Service: "qan",
		Cmd:     "StartTool",
		Data:    configBytes,
	}
	cmdBytes, _ := json.Marshal(cmd)

	// Send the StartTool cmd to the API which relays it to the agent, then
	// relays the agent's reply back to here.
	url := a.api.URL(a.config.ServerAddress+":"+proto.DEFAULT_QAN_API_PORT, "agents", agentId, "cmd")
	resp, content, err := a.api.Put(url, cmdBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return a.api.Error("PUT", url, resp.StatusCode, http.StatusOK, content)
	}

	return nil
}

func (a *Admin) stopQAN(agentId string, in proto.Instance) error {
	cmd := proto.Cmd{
		User:    "pmm-admin@" + a.api.Hostname(),
		Service: "qan",
		Cmd:     "StopTool",
		Data:    []byte(in.UUID),
	}
	cmdBytes, _ := json.Marshal(cmd)

	// Send the StartTool cmd to the API which relays it to the agent, then
	// relays the agent's reply back to here.
	url := a.api.URL(a.config.ServerAddress+":"+proto.DEFAULT_QAN_API_PORT, "agents", agentId, "cmd")
	resp, content, err := a.api.Put(url, cmdBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return a.api.Error("PUT", url, resp.StatusCode, http.StatusOK, content)
	}

	return nil
}

func (a *Admin) serviceExists(host string, job string) (bool, error) {
	// Check if node service exists on Consul
	url := a.api.URL(a.config.ServerAddress+":"+CONSUL_PORT, "v1", "catalog", "node", host)
	resp, bytes, err := a.api.Get(url)
	if err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, a.api.Error("GET", url, resp.StatusCode, http.StatusOK, bytes)
	}

	var data interface{}
	if string(bytes) == "null" {
		// Node does not exist
		return false, nil
	} else if err = json.Unmarshal(bytes, &data); err != nil {
		// Node exists
		return false, err
	}

	// Check service
	if val, ok := data.(map[string]interface{})["Services"]; ok {
		if _, ok := val.(map[string]interface{})[job]; ok {
			return true, nil
		}
	}
	// Node exists but no service
	return false, nil
}
