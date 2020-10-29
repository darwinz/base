package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/moov-io/base"
	"github.com/moov-io/base/docker"

	kitprom "github.com/go-kit/kit/metrics/prometheus"
	gomysql "github.com/go-sql-driver/mysql"
	"github.com/ory/dockertest/v3"
	dc "github.com/ory/dockertest/v3/docker"
	stdprom "github.com/prometheus/client_golang/prometheus"

	"github.com/moov-io/base/log"
)

var (
	mysqlConnections = kitprom.NewGaugeFrom(stdprom.GaugeOpts{
		Name: "mysql_connections",
		Help: "How many MySQL connections and what status they're in.",
	}, []string{"state"})

	// mySQLErrDuplicateKey is the error code for duplicate entries
	// https://dev.mysql.com/doc/refman/8.0/en/server-error-reference.html#error_er_dup_entry
	mySQLErrDuplicateKey uint16 = 1062

	maxActiveMySQLConnections = func() int {
		if v := os.Getenv("MYSQL_MAX_CONNECTIONS"); v != "" {
			if n, _ := strconv.ParseInt(v, 10, 32); n > 0 {
				return int(n)
			}
		}
		return 16
	}()
)

type discardLogger struct{}

func (l discardLogger) Print(v ...interface{}) {}

func init() {
	gomysql.SetLogger(discardLogger{})
}

type mysql struct {
	dsn    string
	logger log.Logger

	connections *kitprom.Gauge
}

func (my *mysql) Connect(ctx context.Context) (*sql.DB, error) {
	db, err := sql.Open("mysql", my.dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxActiveMySQLConnections)

	// Check out DB is up and working
	if err := db.Ping(); err != nil {
		return nil, err
	}

	// Setup metrics after the database is setup
	go func() {
		t := time.NewTicker(1 * time.Minute)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				stats := db.Stats()
				my.connections.With("state", "idle").Set(float64(stats.Idle))
				my.connections.With("state", "inuse").Set(float64(stats.InUse))
				my.connections.With("state", "open").Set(float64(stats.OpenConnections))
			}
		}
	}()

	return db, nil
}

func mysqlConnection(logger log.Logger, user, pass string, address string, database string) *mysql {
	timeout := "30s"
	if v := os.Getenv("MYSQL_TIMEOUT"); v != "" {
		timeout = v
	}
	params := fmt.Sprintf("timeout=%s&charset=utf8mb4&parseTime=true&sql_mode=ALLOW_INVALID_DATES", timeout)
	dsn := fmt.Sprintf("%s:%s@%s/%s?%s", user, pass, address, database, params)
	return &mysql{
		dsn:         dsn,
		logger:      logger,
		connections: mysqlConnections,
	}
}

// TestMySQLDB is a wrapper around sql.DB for MySQL connections designed for tests to provide
// a clean database for each testcase.  Callers should cleanup with Close() when finished.
type TestMySQLDB struct {
	*sql.DB
	name     string
	shutdown func() // context shutdown func
	t        *testing.T
}

func (r *TestMySQLDB) Close() error {
	r.shutdown()

	// Verify all connections are closed before closing DB
	if conns := r.DB.Stats().OpenConnections; conns != 0 {
		require.FailNow(r.t, ErrOpenConnections{
			Database:       "mysql",
			NumConnections: conns,
		}.Error())
	}

	_, err := r.DB.Exec(fmt.Sprintf("drop database %s", r.name))
	if err != nil {
		return err
	}

	if err := r.DB.Close(); err != nil {
		return err
	}

	return nil
}

var SharedMySQL mySQLServer

type mySQLServer struct {
	Config *DatabaseConfig

	start     sync.Once
	container *dockertest.Resource
}

// Start starts MySQL server or finds running server (container) we do not stop
// MySQL server as we can re-use same container during multuple test runs. You
// can safely stop/remove MySQL container manually.
func (m *mySQLServer) Start() error {
	var err error

	m.start.Do(func() {
		m.Config, m.container, err = RunMySQLDockerInstance(&DatabaseConfig{})
	})

	return err
}

// Stop stops container and removes linked volumes
// We don't Stop MySQL to reduce startup time for the next test runs
func (m *mySQLServer) Stop() error {
	return m.container.Close()
}

// CreateTestMySQLDB returns a TestMySQLDB which can be used in tests
// as a clean mysql database. All migrations are ran on the db before.
//
// Callers should call close on the returned *TestMySQLDB.
func CreateTestMySQLDB(t *testing.T) *TestMySQLDB {
	if testing.Short() {
		t.Skip("-short flag enabled")
	}
	if !docker.Enabled() {
		t.Skip("Docker not enabled")
	}

	err := SharedMySQL.Start()
	require.NoError(t, err)

	dbName, err := CreateTemporaryDatabase(SharedMySQL.Config)
	require.NoError(t, err)

	dbConfig := &DatabaseConfig{
		DatabaseName: dbName,
		MySQL:        SharedMySQL.Config.MySQL,
	}

	logger := log.NewNopLogger()
	ctx, cancelFunc := context.WithCancel(context.Background())
	db, err := NewAndMigrate(ctx, logger, *dbConfig)
	if err != nil {
		t.Fatal(err)
	}

	// Don't allow idle connections so we can verify all are closed at the end of testing
	db.SetMaxIdleConns(0)

	return &TestMySQLDB{
		DB:       db,
		name:     dbName,
		shutdown: cancelFunc,
		t:        t,
	}
}

// We connect as root to MySQL server and create database with random name to
// run our migrations on it later.
func CreateTemporaryDatabase(config *DatabaseConfig) (string, error) {
	dsn := fmt.Sprintf("%s:%s@%s/", "root", config.MySQL.Password, config.MySQL.Address)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return "", err
	}
	defer db.Close()

	dbName := "test" + base.ID()

	_, err = db.ExecContext(context.Background(), fmt.Sprintf("create database %s", dbName))
	if err != nil {
		return "", err
	}

	_, err = db.ExecContext(context.Background(), fmt.Sprintf("grant all on %s.* to '%s'@'%%'", dbName, config.MySQL.User))
	if err != nil {
		return "", err
	}

	return dbName, nil
}

func RunMySQLDockerInstance(config *DatabaseConfig) (*DatabaseConfig, *dockertest.Resource, error) {
	if config.MySQL == nil {
		config.MySQL = &MySQLConfig{}
	}

	if config.MySQL.User == "" {
		config.MySQL.User = "moov"
	}

	if config.MySQL.Password == "" {
		config.MySQL.Password = "secret"
	}

	resource, err := findOrLaunchMySQLContainer(config)
	if err != nil {
		return nil, nil, err
	}
	address := fmt.Sprintf("tcp(localhost:%s)", resource.GetPort("3306/tcp"))

	return &DatabaseConfig{
		DatabaseName: config.DatabaseName,
		MySQL: &MySQLConfig{
			Address:  address,
			User:     config.MySQL.User,
			Password: config.MySQL.Password,
		},
	}, resource, nil
}

func findOrLaunchMySQLContainer(config *DatabaseConfig) (*dockertest.Resource, error) {
	var containerName = "mysql-test-container"
	var resource *dockertest.Resource
	var err error

	pool, err := dockertest.NewPool("")
	if err != nil {
		return nil, err
	}

	_, err = pool.RunWithOptions(&dockertest.RunOptions{
		Name:       containerName,
		Repository: "vaulty/mysql-volumeless",
		Tag:        "8.0",
		Env: []string{
			fmt.Sprintf("MYSQL_USER=%s", config.MySQL.User),
			fmt.Sprintf("MYSQL_PASSWORD=%s", config.MySQL.Password),
			fmt.Sprintf("MYSQL_ROOT_PASSWORD=%s", config.MySQL.Password),
		},
	})

	if err != nil && !errors.Is(err, dc.ErrContainerAlreadyExists) {
		return nil, err
	}

	// look for running container
	resource, found := pool.ContainerByName(containerName)
	if !found {
		return nil, errors.New("failed to launch (or find) MySQL container")
	}

	address := fmt.Sprintf("tcp(localhost:%s)", resource.GetPort("3306/tcp"))

	dbURL := fmt.Sprintf("%s:%s@%s/%s",
		config.MySQL.User,
		config.MySQL.Password,
		address,
		config.DatabaseName,
	)

	err = pool.Retry(func() error {
		db, err := sql.Open("mysql", dbURL)
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	})
	if err != nil {
		resource.Close()
		return nil, err
	}

	return resource, nil
}

// MySQLUniqueViolation returns true when the provided error matches the MySQL code
// for duplicate entries (violating a unique table constraint).
func MySQLUniqueViolation(err error) bool {
	match := strings.Contains(err.Error(), fmt.Sprintf("Error %d: Duplicate entry", mySQLErrDuplicateKey))
	if e, ok := err.(*gomysql.MySQLError); ok {
		return match || e.Number == mySQLErrDuplicateKey
	}
	return match
}
