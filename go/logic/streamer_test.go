package logic

import (
	"context"
	"database/sql"
	gosql "database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/github/gh-ost/go/binlog"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mysql"

	"github.com/testcontainers/testcontainers-go/wait"
	"golang.org/x/sync/errgroup"
)

type EventsStreamerTestSuite struct {
	suite.Suite

	mysqlContainer testcontainers.Container
	db             *gosql.DB
}

func (suite *EventsStreamerTestSuite) SetupSuite() {
	ctx := context.Background()
	mysqlContainer, err := mysql.Run(ctx,
		testMysqlContainerImage,
		mysql.WithDatabase(testMysqlDatabase),
		mysql.WithUsername(testMysqlUser),
		mysql.WithPassword(testMysqlPass),
		testcontainers.WithWaitStrategy(wait.ForExposedPort()),
	)
	suite.Require().NoError(err)

	suite.mysqlContainer = mysqlContainer
	dsn, err := mysqlContainer.ConnectionString(ctx)
	suite.Require().NoError(err)

	db, err := gosql.Open("mysql", dsn)
	suite.Require().NoError(err)

	suite.db = db
}

func (suite *EventsStreamerTestSuite) TeardownSuite() {
	suite.Assert().NoError(suite.db.Close())
	suite.Assert().NoError(testcontainers.TerminateContainer(suite.mysqlContainer))
}

func (suite *EventsStreamerTestSuite) SetupTest() {
	ctx := context.Background()

	_, err := suite.db.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+testMysqlDatabase)
	suite.Require().NoError(err)
}

func (suite *EventsStreamerTestSuite) TearDownTest() {
	ctx := context.Background()

	_, err := suite.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+getTestTableName())
	suite.Require().NoError(err)
	_, err = suite.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+getTestGhostTableName())
	suite.Require().NoError(err)
}

func (suite *EventsStreamerTestSuite) TestStreamEvents() {
	ctx := context.Background()

	_, err := suite.db.ExecContext(ctx, fmt.Sprintf("CREATE TABLE %s (id INT PRIMARY KEY, name VARCHAR(255))", getTestTableName()))
	suite.Require().NoError(err)

	connectionConfig, err := getTestConnectionConfig(ctx, suite.mysqlContainer)
	suite.Require().NoError(err)

	migrationContext := newTestMigrationContext()
	migrationContext.ApplierConnectionConfig = connectionConfig
	migrationContext.InspectorConnectionConfig = connectionConfig
	migrationContext.SetConnectionConfig("innodb")

	streamer := NewEventsStreamer(migrationContext)

	err = streamer.InitDBConnections()
	suite.Require().NoError(err)
	defer streamer.Close()
	defer streamer.Teardown()

	streamCtx, cancel := context.WithCancel(context.Background())

	dmlEvents := make([]*binlog.BinlogDMLEvent, 0)
	err = streamer.AddListener(false, testMysqlDatabase, testMysqlTableName, func(event *binlog.BinlogDMLEvent) error {
		dmlEvents = append(dmlEvents, event)

		// Stop once we've collected three events
		if len(dmlEvents) == 3 {
			cancel()
		}

		return nil
	})
	suite.Require().NoError(err)

	group := errgroup.Group{}
	group.Go(func() error {
		return streamer.StreamEvents(func() bool {
			return streamCtx.Err() != nil
		})
	})

	group.Go(func() error {
		var err error

		_, err = suite.db.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (id, name) VALUES (1, 'foo')", getTestTableName()))
		if err != nil {
			return err
		}

		_, err = suite.db.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (id, name) VALUES (2, 'bar')", getTestTableName()))
		if err != nil {
			return err
		}

		_, err = suite.db.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (id, name) VALUES (3, 'baz')", getTestTableName()))
		if err != nil {
			return err
		}

		// Bug: Need to write fourth event to hit the canStopStreaming function again
		_, err = suite.db.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (id, name) VALUES (4, 'qux')", getTestTableName()))
		if err != nil {
			return err
		}

		return nil
	})

	err = group.Wait()
	suite.Require().NoError(err)

	suite.Require().Len(dmlEvents, 3)
}

func (suite *EventsStreamerTestSuite) TestStreamEventsAutomaticallyReconnects() {
	ctx := context.Background()
	_, err := suite.db.ExecContext(ctx, fmt.Sprintf("CREATE TABLE %s (id INT PRIMARY KEY, name VARCHAR(255))", getTestTableName()))
	suite.Require().NoError(err)

	connectionConfig, err := getTestConnectionConfig(ctx, suite.mysqlContainer)
	suite.Require().NoError(err)

	migrationContext := newTestMigrationContext()
	migrationContext.ApplierConnectionConfig = connectionConfig
	migrationContext.InspectorConnectionConfig = connectionConfig
	migrationContext.SetConnectionConfig("innodb")

	streamer := NewEventsStreamer(migrationContext)

	err = streamer.InitDBConnections()
	suite.Require().NoError(err)
	defer streamer.Close()
	defer streamer.Teardown()

	streamCtx, cancel := context.WithCancel(context.Background())

	dmlEvents := make([]*binlog.BinlogDMLEvent, 0)
	err = streamer.AddListener(false, testMysqlDatabase, testMysqlTableName, func(event *binlog.BinlogDMLEvent) error {
		dmlEvents = append(dmlEvents, event)

		// Stop once we've collected three events
		if len(dmlEvents) == 3 {
			cancel()
		}

		return nil
	})
	suite.Require().NoError(err)

	group := errgroup.Group{}
	group.Go(func() error {
		return streamer.StreamEvents(func() bool {
			return streamCtx.Err() != nil
		})
	})

	group.Go(func() error {
		var err error

		_, err = suite.db.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (id, name) VALUES (1, 'foo')", getTestTableName()))
		if err != nil {
			return err
		}

		_, err = suite.db.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (id, name) VALUES (2, 'bar')", getTestTableName()))
		if err != nil {
			return err
		}

		var currentConnectionId int
		err = suite.db.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&currentConnectionId)
		if err != nil {
			return err
		}

		//nolint:execinquery
		rows, err := suite.db.Query("SHOW FULL PROCESSLIST")
		if err != nil {
			return err
		}
		defer rows.Close()

		connectionIdsToKill := make([]int, 0)

		var id, stateTime int
		var user, host, dbName, command, state, info sql.NullString
		for rows.Next() {
			err = rows.Scan(&id, &user, &host, &dbName, &command, &stateTime, &state, &info)
			if err != nil {
				return err
			}

			fmt.Printf("id: %d, user: %s, host: %s, dbName: %s, command: %s, time: %d, state: %s, info: %s\n", id, user.String, host.String, dbName.String, command.String, stateTime, state.String, info.String)

			if id != currentConnectionId && user.String == testMysqlUser {
				connectionIdsToKill = append(connectionIdsToKill, id)
			}
		}

		if err := rows.Err(); err != nil {
			return err
		}

		for _, connectionIdToKill := range connectionIdsToKill {
			_, err = suite.db.ExecContext(ctx, "KILL ?", connectionIdToKill)
			if err != nil {
				return err
			}
		}

		// Bug: We need to wait here for the streamer to reconnect
		time.Sleep(time.Second * 2)

		_, err = suite.db.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (id, name) VALUES (3, 'baz')", getTestTableName()))
		if err != nil {
			return err
		}

		// Bug: Need to write fourth event to hit the canStopStreaming function again
		_, err = suite.db.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (id, name) VALUES (4, 'qux')", getTestTableName()))
		if err != nil {
			return err
		}

		return nil
	})

	err = group.Wait()
	suite.Require().NoError(err)

	suite.Require().Len(dmlEvents, 3)
}

func TestEventsStreamer(t *testing.T) {
	suite.Run(t, new(EventsStreamerTestSuite))
}
