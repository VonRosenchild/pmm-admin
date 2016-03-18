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
	"database/sql"
	"fmt"

	_ "github.com/go-sql-driver/mysql"
	"github.com/percona/go-mysql/dsn"
)

const (
	DEFAULT_MYSQL_USER = "pmm"
	DEFAULT_MYSQL_PASS = "percona2016"
)

type MySQLConn struct {
	userDSN      dsn.DSN
	oldPasswords bool
	maxUserConn  int64
	agentUser    string
	agentPass    string
}

func NewMySQLConn(userDSN dsn.DSN, agentUser, agentPass string, oldPasswords bool, maxUserConn int64) *MySQLConn {
	userDSN.Params = []string{dsn.ParseTimeParam}
	if oldPasswords {
		userDSN.Params = append(userDSN.Params, dsn.OldPasswordsParam)
	}

	m := &MySQLConn{
		userDSN:      userDSN,
		agentUser:    agentUser,
		agentPass:    agentPass,
		oldPasswords: oldPasswords,
		maxUserConn:  maxUserConn,
	}
	return m
}

func MakeGrant(dsn dsn.DSN, mysqlMaxUserConns int64) []string {
	host := "%"
	if dsn.Socket != "" || dsn.Hostname == "localhost" {
		host = "localhost"
	} else if dsn.Hostname == "127.0.0.1" {
		host = "127.0.0.1"
	}
	// Creating/updating a user's password doesn't work correctly if old_passwords is active.
	// Just in case, disable it for this session
	grants := []string{
		"SET SESSION old_passwords=0",
		fmt.Sprintf("GRANT SUPER, PROCESS, USAGE, SELECT ON *.* TO '%s'@'%s' IDENTIFIED BY '%s' WITH MAX_USER_CONNECTIONS %d",
			dsn.Username, host, dsn.Password, mysqlMaxUserConns),
		fmt.Sprintf("GRANT UPDATE, DELETE, DROP ON performance_schema.* TO '%s'@'%s' IDENTIFIED BY '%s' WITH MAX_USER_CONNECTIONS %d",
			dsn.Username, host, dsn.Password, mysqlMaxUserConns),
	}
	return grants
}

func (m *MySQLConn) AgentDSN() (agentDSN dsn.DSN, err error) {
	// USING IMPLICIT RETURN

	if m.agentUser != "" {
		// Use the given agent MySQL user.
		agentDSN = m.userDSN
		agentDSN.Username = m.agentUser
		agentDSN.Password = m.agentPass
		err = m.TestConnection(agentDSN)
	} else {
		// Create a new agent MySQL user.
		agentDSN, err = m.createAgentMySQLUser(m.userDSN)
	}

	return // implicit return
}

func (m *MySQLConn) createAgentMySQLUser(userDSN dsn.DSN) (dsn.DSN, error) {
	// First verify that we can connect to MySQL. Should be root/super user.
	db, err := sql.Open("mysql", userDSN.String())
	if err != nil {
		return dsn.DSN{}, err
	}
	defer db.Close()

	// Agent DSN has same host:port or socket, but different user and pass.
	agentDSN := userDSN
	agentDSN.Username = DEFAULT_MYSQL_USER
	agentDSN.Password = DEFAULT_MYSQL_PASS

	// Create the agent MySQL user with necessary privs.
	grants := MakeGrant(agentDSN, m.maxUserConn)
	for _, grant := range grants {
		_, err := db.Exec(grant)
		if err != nil {
			return dsn.DSN{}, fmt.Errorf("cannot execute %s: %s", grant, err)
		}
	}

	// Go MySQL driver resolves localhost to 127.0.0.1 but localhost is a special
	// value for MySQL, so 127.0.0.1 may not work with a grant @localhost, so we
	// add a 2nd grant @127.0.0.1 to be sure.
	if agentDSN.Hostname == "localhost" {
		agentDSN_127_1 := agentDSN
		agentDSN_127_1.Hostname = "1271.0.0.1"
		grants := MakeGrant(agentDSN_127_1, m.maxUserConn)
		for _, grant := range grants {
			_, err := db.Exec(grant)
			if err != nil {
				return dsn.DSN{}, fmt.Errorf("cannot execute %s: %s", grant, err)
			}
		}
	}

	// Verify new agent MySQL user works. If this fails, the agent DSN or grant
	// statemetns are wrong.
	if err := m.TestConnection(agentDSN); err != nil {
		return dsn.DSN{}, err
	}

	return agentDSN, nil
}

func (m *MySQLConn) TestConnection(newDSN dsn.DSN) error {
	var err error
	var db *sql.DB

	// Make logical sql.DB connection, not an actual MySQL connection...
	db, err = sql.Open("mysql", newDSN.String())
	if err != nil {
		return fmt.Errorf("cannot connect to MySQL %s: %s", dsn.HidePassword(newDSN.String()), err)
	}
	defer db.Close()

	// Must call sql.DB.Ping to test actual MySQL connection.
	if err = db.Ping(); err != nil {
		return fmt.Errorf("cannot connect to MySQL %s: %s", dsn.HidePassword(newDSN.String()), err)
	}

	return nil
}

func (m *MySQLConn) Info(infoDSN dsn.DSN) (map[string]string, error) {
	db, err := sql.Open("mysql", infoDSN.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var (
		hostname string
		port     string
		distro   string
		version  string
	)
	err = db.QueryRow("SELECT @@hostname, @@port, @@version_comment, @@version").Scan(
		&hostname, &port, &distro, &version)
	if err != nil {
		return nil, err
	}
	info := map[string]string{
		"hostname": hostname,
		"port":     port,
		"distro":   distro,
		"version":  version,
	}
	return info, nil
}
