package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	stdmysql "github.com/go-sql-driver/mysql"
	"github.com/hashicorp/errwrap"
	dbplugin "github.com/hashicorp/vault/sdk/database/dbplugin/v5"
	"github.com/hashicorp/vault/sdk/database/helper/credsutil"
	"github.com/hashicorp/vault/sdk/database/helper/dbutil"
	"github.com/hashicorp/vault/sdk/helper/strutil"
)

const (
	defaultMysqlRevocationStmts = `
		REVOKE ALL PRIVILEGES, GRANT OPTION FROM '{{name}}'@'%'; 
		DROP USER '{{name}}'@'%'
	`

	defaultMySQLRotateCredentialsSQL = `
		ALTER USER '{{username}}'@'%' IDENTIFIED BY '{{password}}';
	`

	mySQLTypeName = "mysql"
)

// Modern
// v_  displayname_ metadata_ uuid_time
// -2- -----14----- ----11--- ----5----
// ---------------32------------------
//
// Legacy
// v_  displayname_ metadata_ uuid_time
// -2- -----6------ ----5---- ----3----
// ---------------16------------------

var (
	DisplayNameLen       int = 13
	LegacyDisplayNameLen int = 5
	MetadataLen          int = 10
	LegacyMetadataLen    int = 4
	UsernameLen          int = 32
	LegacyUsernameLen    int = 16
)

var _ dbplugin.Database = (*MySQL)(nil)

type MySQL struct {
	*mySQLConnectionProducer
	legacy bool
}

// New implements builtinplugins.BuiltinFactory
func New(legacy bool) func() (interface{}, error) {
	return func() (interface{}, error) {
		db := new(legacy)
		// Wrap the plugin with middleware to sanitize errors
		dbType := dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.SecretValues)

		return dbType, nil
	}
}

func new(legacy bool) *MySQL {
	connProducer := &mySQLConnectionProducer{}

	return &MySQL{
		mySQLConnectionProducer: connProducer,
		legacy:                  legacy,
	}
}

func (m *MySQL) Type() (string, error) {
	return mySQLTypeName, nil
}

func (m *MySQL) getConnection(ctx context.Context) (*sql.DB, error) {
	db, err := m.Connection(ctx)
	if err != nil {
		return nil, err
	}

	return db.(*sql.DB), nil
}

func (m *MySQL) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	err := m.mySQLConnectionProducer.Initialize(ctx, req.Config, req.VerifyConnection)
	if err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	resp := dbplugin.InitializeResponse{
		Config: req.Config,
	}
	return resp, nil
}

func (m *MySQL) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	if len(req.Statements.Commands) == 0 {
		return dbplugin.NewUserResponse{}, dbutil.ErrEmptyCreationStatement
	}

	username, err := m.generateUsername(req)
	if err != nil {
		return dbplugin.NewUserResponse{}, err
	}

	password := req.Password

	expirationStr := req.Expiration.Format("2006-01-02 15:04:05-0700")

	queryMap := map[string]string{
		"name":       username,
		"username":   username,
		"password":   password,
		"expiration": expirationStr,
	}

	if err := m.executePreparedStatementsWithMap(ctx, req.Statements.Commands, queryMap); err != nil {
		return dbplugin.NewUserResponse{}, err
	}

	resp := dbplugin.NewUserResponse{
		Username: username,
	}
	return resp, nil
}

func (m *MySQL) generateUsername(req dbplugin.NewUserRequest) (string, error) {
	var dispNameLen, roleNameLen, maxLen int

	if m.legacy {
		dispNameLen = LegacyDisplayNameLen
		roleNameLen = LegacyMetadataLen
		maxLen = LegacyUsernameLen
	} else {
		dispNameLen = DisplayNameLen
		roleNameLen = MetadataLen
		maxLen = UsernameLen
	}

	username, err := credsutil.GenerateUsername(
		credsutil.DisplayName(req.UsernameConfig.DisplayName, dispNameLen),
		credsutil.RoleName(req.UsernameConfig.RoleName, roleNameLen),
		credsutil.MaxLength(maxLen),
	)
	if err != nil {
		return "", errwrap.Wrapf("error generating username: {{err}}", err)
	}

	return username, nil
}

func (m *MySQL) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	// Grab the read lock
	m.Lock()
	defer m.Unlock()

	// Get the connection
	db, err := m.getConnection(ctx)
	if err != nil {
		return dbplugin.DeleteUserResponse{}, err
	}

	revocationStmts := req.Statements.Commands
	// Use a default SQL statement for revocation if one cannot be fetched from the role
	if len(revocationStmts) == 0 {
		revocationStmts = []string{defaultMysqlRevocationStmts}
	}

	// Start a transaction
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return dbplugin.DeleteUserResponse{}, err
	}
	defer tx.Rollback()

	for _, stmt := range revocationStmts {
		for _, query := range strutil.ParseArbitraryStringSlice(stmt, ";") {
			query = strings.TrimSpace(query)
			if len(query) == 0 {
				continue
			}

			// This is not a prepared statement because not all commands are supported
			// 1295: This command is not supported in the prepared statement protocol yet
			// Reference https://mariadb.com/kb/en/mariadb/prepare-statement/
			query = strings.Replace(query, "{{name}}", req.Username, -1)
			query = strings.Replace(query, "{{username}}", req.Username, -1)
			_, err = tx.ExecContext(ctx, query)
			if err != nil {
				return dbplugin.DeleteUserResponse{}, err
			}
		}
	}

	// Commit the transaction
	err = tx.Commit()
	return dbplugin.DeleteUserResponse{}, err
}

func (m *MySQL) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Password == nil && req.Expiration == nil {
		return dbplugin.UpdateUserResponse{}, fmt.Errorf("no change requested")
	}

	if req.Password != nil {
		err := m.changeUserPassword(ctx, req.Username, req.Password.NewPassword, req.Password.Statements.Commands)
		if err != nil {
			return dbplugin.UpdateUserResponse{}, fmt.Errorf("failed to change password: %w", err)
		}
	}

	// Expiration change/update is currently a no-op

	return dbplugin.UpdateUserResponse{}, nil
}

func (m *MySQL) changeUserPassword(ctx context.Context, username, password string, rotateStatements []string) error {
	if username == "" || password == "" {
		return errors.New("must provide both username and password")
	}

	if len(rotateStatements) == 0 {
		rotateStatements = []string{defaultMySQLRotateCredentialsSQL}
	}

	queryMap := map[string]string{
		"name":     username,
		"username": username,
		"password": password,
	}

	if err := m.executePreparedStatementsWithMap(ctx, rotateStatements, queryMap); err != nil {
		return err
	}
	return nil
}

// executePreparedStatementsWithMap loops through the given templated SQL statements and
// applies the map to them, interpolating values into the templates, returning
// the resulting username and password
func (m *MySQL) executePreparedStatementsWithMap(ctx context.Context, statements []string, queryMap map[string]string) error {
	// Grab the lock
	m.Lock()
	defer m.Unlock()

	// Get the connection
	db, err := m.getConnection(ctx)
	if err != nil {
		return err
	}
	// Start a transaction
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	// Execute each query
	for _, stmt := range statements {
		for _, query := range strutil.ParseArbitraryStringSlice(stmt, ";") {
			query = strings.TrimSpace(query)
			if len(query) == 0 {
				continue
			}

			query = dbutil.QueryHelper(query, queryMap)

			stmt, err := tx.PrepareContext(ctx, query)
			if err != nil {
				// If the error code we get back is Error 1295: This command is not
				// supported in the prepared statement protocol yet, we will execute
				// the statement without preparing it. This allows the caller to
				// manually prepare statements, as well as run other not yet
				// prepare supported commands. If there is no error when running we
				// will continue to the next statement.
				if e, ok := err.(*stdmysql.MySQLError); ok && e.Number == 1295 {
					_, err = tx.ExecContext(ctx, query)
					if err != nil {
						stmt.Close()
						return err
					}
					continue
				}

				return err
			}
			if _, err := stmt.ExecContext(ctx); err != nil {
				stmt.Close()
				return err
			}
			stmt.Close()
		}
	}

	// Commit the transaction
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}
