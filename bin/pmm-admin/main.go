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
	"regexp"
	"strings"

	"github.com/percona/go-mysql/dsn"
	pmm "github.com/percona/pmm-admin"
)

const (
	DEFAULT_QAN_API_PORT         = "9001"
	DEFAULT_PROM_CONFIG_API_PORT = "9003"
	DEFAULT_CONFIG_FILE          = "/usr/local/percona/pmm.yml"
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
)

var fs *flag.FlagSet
var portNumberRe = regexp.MustCompile(`\.\d+$`)

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
}

var portSuffix *regexp.Regexp = regexp.MustCompile(`:\d+$`)

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
	if len(args) == 0 {
		fmt.Println("No command specified. Run 'pmm-admin help'.")
		os.Exit(1)
	}

	// Handle special command: client|server <addr>. This initializes the config
	// file and sets the client|server address, required for all other commands.
	admin := pmm.NewAdmin()
	if err := admin.LoadConfig(flagConfig); err != nil {
		fmt.Printf("Error reading %s: %s\n", flagConfig, err)
		os.Exit(1)
	}

	cmd := args[0]
	if (cmd == "server" || cmd == "client") && len(args) == 2 {
		addr := args[1]
		var err error
		if cmd == "server" {
			err = admin.SetServer(addr)
		} else {
			err = admin.SetClient(addr)
		}
		if err != nil {
			fmt.Printf("Error setting %s: %s\n", cmd, err)
			os.Exit(1)
		}
		fmt.Printf("OK, %s is %s\n", cmd, addr)
		os.Exit(0)
	}

	// Command is not "server <addr>", so from this point we require that the
	// config file exists and the client and server addresses are set.
	if admin.Client() == "" {
		fmt.Printf("%s exists but the client address has not been set. Run 'pmm-admin client <address>'.\n", flagConfig)
		os.Exit(1)
	}
	if admin.Server() == "" {
		fmt.Printf("%s exists but the server address has not been set. Run 'pmm-admin server <address[:port]>'.\n", flagConfig)
		os.Exit(1)
	}

	api := pmm.NewAPI(nil)
	admin.SetAPI(api)

	// Execute the command.
	switch cmd {
	case "help":
		if len(args) == 1 {
			fmt.Println("Usage: pmm-admin [options] <command> [command args]\n\n" +
				"Commands: add, client, list, remove, server\n\n" +
				"  <> = required\n" +
				"  [] = optional\n" +
				"  [options] (-user, -password, etc.) must precede the <command>\n\n" +
				"Example:\n" +
				"  pmm-admin -agent-user percona -password percona add mysql\n\n" +
				"Run 'pmm-admin help options' to list [options]\n" +
				"Run 'pmm-admin help <command>' for command-specific help\n")
		} else {
			cmd := args[1]
			switch cmd {
			case "options":
				fs.PrintDefaults()
			case "add":
				fmt.Printf("Usage: pmm-admin [options] add <instance type>\n\n" +
					"Instance types:\n" +
					"  os     Add local OS instance and start monitoring\n" +
					"  mysql  Add local MySQL instance and start monitoring\n\n" +
					"When adding a MySQL instance, specify -agent-user and -agent-password" +
					" to use an existing MySQL user. Else, the agent MySQL user will be created" +
					" automatically.\n")
			case "list":
				fmt.Printf("Usage: pmm-admin list\n\nList OS and local MySQL instances being monitored.\n")
			case "client":
				fmt.Printf("Usage: pmm-admin client [address]\n\nPrints address of this server, or sets it if [address] given.\n")
			case "server":
				fmt.Printf("Usage: pmm-admin server [address[:port]]\n\nPrints address of PMM server, or sets it if [address] given.\n")
			default:
				fmt.Printf("Unknown comand: %s\n", cmd)
			}
		}
		os.Exit(0)
	case "client":
		fmt.Println(admin.Client())
	case "server":
		fmt.Println(admin.Server())
	case "list", "ls":
		list, err := admin.List()
		if err != nil {
			fmt.Printf("Error getting list: %s\n", err)
			os.Exit(1)
		}
		linefmt := "%7s %7s %7s %32s %s\n"
		fmt.Printf(linefmt, "TYPE", "METRICS", "QUERIES", "UUID", "NAME")
		fmt.Printf(linefmt, "-------", "-------", "-------", "--------------------------------", "----")
		for instanceType, instances := range list {
			for _, in := range instances {
				fmt.Printf(linefmt, instanceType, in.Metrics, in.Queries, in.UUID, in.Name)
			}
		}
	case "add":
		if len(args[1:]) != 1 {
			fmt.Printf("Too many command args: '%s', expected 'add <instance type>'\n", strings.Join(args, " "))
			os.Exit(1)
		}
		instanceType := args[1]
		switch instanceType {
		case "os":
		case "mysql":
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

			if err := admin.AddMySQL(name, agentDSN.String(), flagQuerySource, info); err != nil {
				fmt.Printf("Error adding MySQL: %s\n", err)
				os.Exit(1)
			}

			fmt.Printf("OK, now monitoring MySQL %s using DSN %s\n", name, dsn.HidePassword(agentDSN.String()))
		default:
			fmt.Printf("Invalid instance type: %s\n", instanceType)
		}
	case "rm", "remove":
	default:
		fmt.Printf("Unknown command: '%s'\n", args[0])
		os.Exit(1)
	}

	os.Exit(0)
}
