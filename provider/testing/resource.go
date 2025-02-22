package testing

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/cloudquery/cq-provider-sdk/provider/execution"

	sq "github.com/Masterminds/squirrel"
	"github.com/cloudquery/cq-provider-sdk/cqproto"
	"github.com/cloudquery/cq-provider-sdk/database"
	"github.com/cloudquery/cq-provider-sdk/migration"
	"github.com/cloudquery/cq-provider-sdk/provider"
	"github.com/cloudquery/cq-provider-sdk/provider/schema"
	"github.com/cloudquery/cq-provider-sdk/testlog"
	"github.com/cloudquery/faker/v3"
	"github.com/georgysavva/scany/pgxscan"
	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/assert"
)

type ResourceTestCase struct {
	Provider *provider.Provider
	Config   string
	// we want it to be parallel by default
	NotParallel bool
	// ParallelFetchingLimit limits parallel resources fetch at a time
	ParallelFetchingLimit uint64
	// SkipIgnoreInTest flag which detects if schema.Table or schema.Column should be ignored
	SkipIgnoreInTest bool
}

func init() {
	_ = faker.SetRandomMapAndSliceMinSize(1)
	_ = faker.SetRandomMapAndSliceMaxSize(1)
}

func TestResource(t *testing.T, resource ResourceTestCase) {
	if !resource.NotParallel {
		t.Parallel()
	}
	t.Helper()

	// No need for configuration or db connection, get it out of the way first
	// testTableIdentifiersForProvider(t, resource.Provider)

	conn, err := setupDatabase()
	if err != nil {
		t.Fatal(err)
	}

	l := testlog.New(t)
	l.SetLevel(hclog.Info)
	resource.Provider.Logger = l
	tableCreator := migration.NewTableCreator(l, schema.PostgresDialect{})

	for _, table := range resource.Provider.ResourceMap {
		if err := dropAndCreateTable(context.Background(), tableCreator, conn, table, nil); err != nil {
			assert.FailNow(t, fmt.Sprintf("failed to create tables %s", table.Name), err)
		}
	}

	if err = fetch(t, &resource); err != nil {
		t.Fatal(err)
	}
	for _, table := range resource.Provider.ResourceMap {
		verifyNoEmptyColumns(t, table, conn, resource.SkipIgnoreInTest)
	}

}

// fetch - fetches resources from the cloud and puts them into database. database config can be specified via DATABASE_URL env variable
func fetch(t *testing.T, resource *ResourceTestCase) error {
	t.Helper()
	resourceNames := make([]string, 0, len(resource.Provider.ResourceMap))
	for name := range resource.Provider.ResourceMap {
		resourceNames = append(resourceNames, name)
	}

	t.Logf("fetch resources %v", resourceNames)

	if _, err := resource.Provider.ConfigureProvider(context.Background(), &cqproto.ConfigureProviderRequest{
		CloudQueryVersion: "",
		Connection: cqproto.ConnectionDetails{DSN: getEnv("DATABASE_URL",
			"host=localhost user=postgres password=pass DB.name=postgres port=5432")},
		Config: []byte(resource.Config),
	}); err != nil {
		return err
	}

	var resourceSender = &testResourceSender{
		Errors: []string{},
	}

	if err := resource.Provider.FetchResources(context.Background(),
		&cqproto.FetchResourcesRequest{
			Resources:             resourceNames,
			ParallelFetchingLimit: resource.ParallelFetchingLimit,
		},
		resourceSender,
	); err != nil {
		return err
	}

	if len(resourceSender.Errors) > 0 {
		return fmt.Errorf("error/s occur during test, %s", strings.Join(resourceSender.Errors, ", "))
	}

	return nil
}

func verifyNoEmptyColumns(t *testing.T, table *schema.Table, conn pgxscan.Querier, shouldSkipIgnoreInTest bool) {
	t.Helper()
	t.Run(table.Name, func(t *testing.T) {
		t.Helper()

		if !shouldSkipIgnoreInTest && table.IgnoreInTests {
			t.Skipf("table %s marked as IgnoreInTest. Skipping...", table.Name)
		}
		s := sq.StatementBuilder.
			PlaceholderFormat(sq.Dollar).
			Select(fmt.Sprintf("json_agg(%s)", table.Name)).
			From(table.Name)
		query, args, err := s.ToSql()
		if err != nil {
			t.Fatal(err)
		}
		var data []map[string]interface{}
		if err := pgxscan.Get(context.Background(), conn, &data, query, args...); err != nil {
			t.Fatal(err)
		}

		if len(data) == 0 {
			t.Errorf("expected to have at least 1 entry at table %s got zero", table.Name)
			return
		}

		nilColumns := map[string]bool{}
		// mark all columns as nil
		for _, c := range table.Columns {
			if shouldSkipIgnoreInTest || !c.IgnoreInTests {
				nilColumns[c.Name] = true
			}
		}

		for _, row := range data {
			for c, v := range row {
				if v != nil {
					// as long as we had one row or result with this column not nil it means the resolver worked
					nilColumns[c] = false
				}
			}
		}

		var nilColumnsArr []string
		for c, v := range nilColumns {
			if v {
				nilColumnsArr = append(nilColumnsArr, c)
			}
		}

		if len(nilColumnsArr) != 0 {
			t.Errorf("found nil column in table %s. columns=%s", table.Name, strings.Join(nilColumnsArr, ","))
		}
		for _, childTable := range table.Relations {
			verifyNoEmptyColumns(t, childTable, conn, shouldSkipIgnoreInTest)
		}
	})
}

func dropAndCreateTable(
	ctx context.Context,
	tableCreator *migration.TableCreator,
	conn execution.QueryExecer,
	table *schema.Table,
	parent *schema.Table) error {

	ups, downs, err := tableCreator.CreateTableDefinitions(ctx, table, parent)

	if err != nil {
		return err
	}

	for _, sql := range downs {
		if err := conn.Exec(ctx, sql); err != nil {
			return err
		}
	}

	for _, sql := range ups {
		if err := conn.Exec(ctx, sql); err != nil {
			return err
		}
	}

	return nil
}

type testResourceSender struct {
	Errors []string
}

func (f *testResourceSender) Send(r *cqproto.FetchResourcesResponse) error {
	if r.Error != "" {
		fmt.Printf(r.Error)
		f.Errors = append(f.Errors, r.Error)
	}
	for _, diag := range r.Summary.Diagnostics {
		f.Errors = append(f.Errors, fmt.Sprintf("resource: %s. summary: %s, details %s", diag.Description().Resource, diag.Description().Summary, diag.Description().Detail))
	}
	return nil
}

var (
	dbConnOnce sync.Once
	pool       execution.QueryExecer
	dbErr      error
)

func setupDatabase() (execution.QueryExecer, error) {
	dbConnOnce.Do(func() {
		pool, dbErr = database.New(context.Background(), hclog.NewNullLogger(), getEnv("DATABASE_URL", "host=localhost user=postgres password=pass DB.name=postgres port=5432"))
		if dbErr != nil {
			return
		}
	})
	return pool, dbErr
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
