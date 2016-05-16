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

package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/percona/go-mysql/dsn"
	pmm "github.com/percona/pmm-admin"
)

const (
	DEFAULT_CONFIG_FILE = "/usr/local/percona/pmm-client/pmm.yml"
)

var (
	flagConfig            string
	flagMySQLUser         string
	flagMySQLPass         string
	flagMySQLHost         string
	flagMySQLPort         string
	flagMySQLSocket       string
	flagMySQLDefaultsFile string
	flagQuerySource       string
	flagMySQLOldPasswords bool
	flagMySQLMaxUserConn  int64
	flagAgentUser         string
	flagAgentPass         string
	flagMongoURI          string
	flagMongoReplSet      string
	flagMongoCluster      string
	flagVersion           bool
	flagStart             bool
)

var fs *flag.FlagSet

func init() {
	fs = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	fs.StringVar(&flagConfig, "config", DEFAULT_CONFIG_FILE, "Config file")

	fs.StringVar(&flagMySQLUser, "user", "", "MySQL username")
	fs.StringVar(&flagMySQLPass, "password", "", "MySQL password")
	fs.StringVar(&flagMySQLHost, "host", "", "MySQL host")
	fs.StringVar(&flagMySQLPort, "port", "", "MySQL port")
	fs.StringVar(&flagMySQLSocket, "socket", "", "MySQL socket file")
	fs.StringVar(&flagMySQLDefaultsFile, "defaults-file", "", "Path to my.cnf")

	fs.StringVar(&flagAgentUser, "agent-user", "", "Existing database username for agent")
	fs.StringVar(&flagAgentPass, "agent-password", "", "Existing database password for agent")

	fs.StringVar(&flagQuerySource, "query-source", "auto", "Where to collect queries: slowlog, perfschema, auto")
	fs.Int64Var(&flagMySQLMaxUserConn, "max-user-connections", 5, "Max number of MySQL connections")
	fs.BoolVar(&flagMySQLOldPasswords, "old-passwords", false, "Old passwords")

	fs.StringVar(&flagMongoURI, "mongodb-uri", "", "MongoDB URI")
	fs.StringVar(&flagMongoReplSet, "mongodb-replset", "", "MongoDB replSet name")
	fs.StringVar(&flagMongoCluster, "mongodb-cluster", "", "MongoDB cluster name")

	fs.BoolVar(&flagVersion, "version", false, "Print version")
	fs.BoolVar(&flagStart, "start", true, "Start monitoring instance after add")
}

func main() {
	// It flag is unknown it exist with os.Exit(10),
	// so exit code=10 is strictly reserved for flags
	// Don't use it anywhere else, as shell script install.sh depends on it
	// NOTE: standard flag.Parse() was using os.Exit(2)
	//       which was the same as returned with ctrl+c
	if err := fs.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			return
		} else {
			fmt.Println(err)
			os.Exit(1)
		}
	}

	// Check for invalid mix of options.
	if flagMySQLSocket != "" && flagMySQLHost != "" {
		fmt.Println("-socket and -host are mutually exclusive")
		os.Exit(1)
	}
	if flagMySQLSocket != "" && flagMySQLPort != "" {
		fmt.Println("-socket and -port are mutually exclusive")
		os.Exit(1)
	}
	if flagQuerySource != "auto" && flagQuerySource != "slowlog" && flagQuerySource != "perfschema" {
		fmt.Printf("Invalid value for -query-source: '%s'\n", flagQuerySource)
		os.Exit(1)
	}
	if (flagAgentUser != "" && flagAgentPass == "") || (flagAgentPass != "" && flagAgentUser == "") {
		fmt.Printf("-agent-user and -agent-password are both required when either one is specified")
		os.Exit(1)
	}

	args := fs.Args()

	// Always show help and version.
	if len(args) > 0 && args[0] == "help" {
		help(args)
		os.Exit(0)
	}
	if flagVersion {
		fmt.Printf("pmm-admin %s\n", pmm.VERSION)
		os.Exit(0)
	}

	admin := pmm.NewAdmin()
	if err := admin.LoadConfig(flagConfig); err != nil {
		fmt.Printf("Error reading %s: %s\n", flagConfig, err)
		os.Exit(1)
	}

	// First arg is the command.
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}

	// Command 'server <addr>' is special: it initializes the config file.
	if cmd == "server" && len(args) == 2 {
		addr := args[1]
		if err := admin.SetServer(addr); err != nil {
			fmt.Printf("Error setting %s: %s\n", cmd, err)
			os.Exit(1)
		}
		fmt.Printf("OK, %s is %s\n", cmd, addr)
		os.Exit(0)
	}

	// If config file doesn't exist, tell user how to get started.
	if !pmm.FileExists(flagConfig) {
		fmt.Printf("%s does not exist. To get started, first run"+
			" 'pmm-admin server <address[:port]>' and 'pmm-admin add os <address>'"+
			" to set the address of the PMM server and this server, respectively."+
			" Run 'pmm-admin help' for more information.\n",
			flagConfig)
		os.Exit(1)
	}

	// A command is required.
	if cmd == "" {
		fmt.Println("No command specified. Run 'pmm-admin help'.")
		os.Exit(1)
	}

	// We have a config file and a command Let's try to execute it.

	// Command is not "server <addr>", so from this point we require that the
	// config file exists and the server addresses is set.
	if admin.Server() == "" {
		fmt.Printf("%s exists but the server address has not been set. Run 'pmm-admin server <address[:port]>'.\n", flagConfig)
		os.Exit(1)
	}

	// Execute the command. Create an API because most commands are just
	// wrappers around various API calls.
	api := pmm.NewAPI(nil)
	admin.SetAPI(api)

	switch cmd {
	case "server":
		fmt.Println(admin.Server())
	case "list", "ls":
		list, err := admin.List()
		if err != nil {
			fmt.Printf("Error getting list: %s\n", err)
			os.Exit(1)
		}
		linefmt := "%10s %-60s %s\n"
		fmt.Printf(linefmt, "TYPE", "NAME", "OPTIONS")
		fmt.Printf(linefmt, strings.Repeat("-", 10), strings.Repeat("-", 60), strings.Repeat("-", 10))
		for instanceType, instances := range list {
			for _, in := range instances {
				var tags []string
				if data, ok := in.Tags.([]interface{}); ok {
					for _, tag := range data {
						tags = append(tags, tag.(string))
					}
				}
				fmt.Printf(linefmt, instanceType, in.Name, strings.Join(tags, ","))
			}
		}
	case "add":
		if len(args) < 2 {
			fmt.Printf("Not enough command args: '%s', expected at least 1: 'add <instance type> [address]'\n", strings.Join(args, " "))
			os.Exit(1)
		}
		if len(args) > 3 {
			fmt.Printf("Too many command args: '%s', expected no more than 2: 'add <instance type> [address]'\n", strings.Join(args, " "))
		}
		instanceType := args[1]
		switch instanceType {
		case "os":
			if len(args) != 3 {
				fmt.Printf("[address] not specified. See 'pmm-admin help add'.\n")
				os.Exit(1)
			}
			addr := args[2]
			if err := admin.AddOS(addr, flagStart, flagMongoReplSet, flagMongoCluster); err != nil {
				if err == pmm.ErrHostConflict {
					hostConflictError("OS", admin.Server())
				} else {
					fmt.Printf("Error adding OS: %s\n", err)
				}
				os.Exit(1)
			}
			if err := admin.LoadConfig(flagConfig); err != nil {
				fmt.Printf("Now monitoring this OS but error reading %s: %s\n", flagConfig, err)
				os.Exit(1)
			}
			os, _ := admin.OS()
			if flagStart {
				fmt.Printf("OK, now monitoring this OS as %s\n", os.Name)
			} else {
				fmt.Printf("OK, added this OS as %s\n", os.Name)
			}
		case "mysql":
			if admin.ClientAddress() == "" {
				fmt.Printf("Add OS first to set client address by running 'pmm-admin add os <address>'\n")
				os.Exit(0)
			}
			userDSN := dsn.DSN{
				DefaultsFile: flagMySQLDefaultsFile,
				Username:     flagMySQLUser,
				Password:     flagMySQLPass,
				Hostname:     flagMySQLHost,
				Port:         flagMySQLPort,
				Socket:       flagMySQLSocket,
			}
			userDSN, err := userDSN.AutoDetect()
			if err != nil && err != dsn.ErrNoSocket {
				fmt.Printf("Cannot auto-detect MySQL: %s. The command will probably fail...\n", err)
			}
			m := pmm.NewMySQLConn(userDSN, flagAgentUser, flagAgentPass, flagMySQLOldPasswords, flagMySQLMaxUserConn)
			agentDSN, err := m.AgentDSN()
			if err != nil {
				fmt.Println("Auto-detected MySQL", dsn.HidePassword(userDSN.String()))
				if flagAgentUser == "" {
					// Failed trying to create agent MySQL user.
					fmt.Printf("Cannot create MySQL user for agent: %s. Use MySQL options (-user, -password, etc.)"+
						" to specify a MySQL user with GRANT privileges. Or, use options -agent-user and -agent-password"+
						" to specify an existing agent MySQL user.\n", err)
				} else {
					// Failed trying to use existing, user-provied agent MySQL user and pass.
					fmt.Printf("Cannot connect to MySQL using the given -agent-user and -agent-password: %s."+
						" Verify that the agent MySQL user exists and has the correct privileges. Specify additional"+
						" MySQL options (-host, -port, -socket, etc.) if needed.", err)
				}
				os.Exit(1)
			}

			// Get MySQL hostname, port, distro, and version. This shouldn't fail
			// because we just verified the agent MySQL user.
			info, err := m.Info(agentDSN)
			if err != nil {
				fmt.Printf("Cannot get MySQL info: %s\n", err)
				os.Exit(1)
			}

			// MySQL is local if the server hostname == MySQL hostname.
			if flagQuerySource == "auto" {
				if info["hostname"] == api.Hostname() {
					flagQuerySource = "slowlog"
				} else {
					flagQuerySource = "perfschema"
				}
			}

			// We need to name this MySQL instance. Default to its hostname, but
			// add ":port" if using a non-standard port because it could indicate
			// that this server is running multiple MySQL instances which requires
			// they each use a different port.
			name := info["hostname"]
			if info["port"] != "3306" {
				name += ":" + info["port"]
			}

			if err := admin.AddMySQL(name, agentDSN.String(), flagQuerySource, flagStart, info); err != nil {
				if err == pmm.ErrHostConflict {
					hostConflictError("MySQL", admin.Server())
				} else {
					fmt.Printf("Error adding MySQL: %s\n", err)
				}
				os.Exit(1)
			}
			if flagStart {
				fmt.Printf("OK, now monitoring MySQL %s using DSN %s\n", name, dsn.HidePassword(agentDSN.String()))
			} else {
				fmt.Printf("OK, added MySQL %s using DSN %s\n", name, dsn.HidePassword(agentDSN.String()))
			}
		case "mongodb":
			if admin.ClientAddress() == "" {
				fmt.Printf("Add OS first to set client address by running 'pmm-admin add os <address>'\n")
				os.Exit(0)
			}
			node, _ := admin.OS()
			if err := admin.AddMongoDB(node.Name, flagStart, flagMongoURI, flagMongoReplSet, flagMongoCluster); err != nil {
				if err == pmm.ErrHostConflict {
					hostConflictError("MongoDB", admin.Server())
				} else {
					fmt.Printf("Error adding MongoDB: %s\n", err)
				}
				os.Exit(1)
			}
			if flagStart {
				fmt.Printf("OK, now monitoring MongoDB %s\n", node.Name)
			} else {
				fmt.Printf("OK, added MongoDB %s\n", node.Name)
			}
		default:
			fmt.Printf("Invalid instance type: %s\n", instanceType)
		}
	case "remove", "rm":
		if len(args[1:]) != 2 {
			fmt.Printf("Too many command args: '%s', expected 'remove <instance type> <name>'\n", strings.Join(args, " "))
			os.Exit(1)
		}
		instanceType := args[1]
		name := args[2]
		switch instanceType {
		case "os":
			if err := admin.RemoveOS(name); err != nil {
				fmt.Printf("Error removing OS %s: %s\n", name, err)
				os.Exit(1)
			}
			fmt.Printf("OK, stopped monitoring this OS\n")
		case "mysql":
			if err := admin.RemoveMySQL(name); err != nil {
				fmt.Printf("Error removing MySQL %s: %s\n", name, err)
				os.Exit(1)
			}
			fmt.Printf("OK, stopped monitoring MySQL %s\n", name)
		case "mongodb":
			if err := admin.RemoveMongoDB(name); err != nil {
				fmt.Printf("Error removing MongoDB %s: %s\n", name, err)
				os.Exit(1)
			}
			fmt.Printf("OK, stopped monitoring MongoDB %s\n", name)
		default:
			fmt.Printf("Invalid instance type: %s\n", instanceType)
		}
	default:
		fmt.Printf("Unknown command: '%s'\n", args[0])
		os.Exit(1)
	}

	os.Exit(0)
}

func help(args []string) {
	if len(args) == 1 {
		fmt.Println("Usage: pmm-admin [options] <command> [command args]\n\n" +
			"Commands: add, list, remove, server\n\n" +
			"  <> = required\n" +
			"  [] = optional\n" +
			"  [options] (-user, -password, etc.) must precede the <command>\n\n" +
			"Example:\n" +
			"  pmm-admin -agent-user percona -agent-password percona add mysql\n\n" +
			"The -config file must exist and be initialized by running the" +
			" 'server <address[:port]>' and 'add os <address>' commands.\n\n" +
			"Run 'pmm-admin help options' to list [options]\n" +
			"Run 'pmm-admin help <command>' for command-specific help\n")
	} else {
		cmd := args[1]
		switch cmd {
		case "options":
			fs.PrintDefaults()
		case "add":
			fmt.Printf("Usage: pmm-admin [options] add <instance type> [address]\n\n" +
				"Instance types:\n" +
				"  os      Add local OS instance and start monitoring\n" +
				"  mysql   Add local MySQL instance and start monitoring\n" +
				"  mongodb Add local MongoDB instance and start monitoring\n\n" +
				"When adding an OS instance (this server), specify its [address].\n\n" +
				"When adding a MySQL instance, specify -agent-user and -agent-password" +
				" to use an existing MySQL user. Else, the agent MySQL user will be created" +
				" automatically.\n")
		case "remove":
			fmt.Printf("Usage: pmm-admin [options] remove <instance type> <name>\n\n" +
				"Instance types:\n" +
				"  os      Stop monitoring local OS instance\n" +
				"  mysql   Stop monitoring local MySQL instance\n\n" +
				"  mongodb Stop monitoring local MongoDB instance\n\n" +
				"Run 'pmm-admin list' to see the name of instances being monitored.\n")
		case "list":
			fmt.Printf("Usage: pmm-admin list\n\nList OS, MySQL or MongoDB instances being monitored.\n")
		case "server":
			fmt.Printf("Usage: pmm-admin server [address[:port]]\n\n" +
				"Prints the address of the PMM server, or sets it if [address] given.\n")
		default:
			fmt.Printf("Unknown comand: %s\n", cmd)
		}
	}
}

func hostConflictError(what, serverAddr string) {
	fmt.Printf("Cannot add %s because a host with the same name but a different address already exists."+
		" This can happen if two clients have the same hostname but different addresses."+
		" To see which %s hosts already exist, run:\n\tpmm-admin list\n",
		what, what)
}
