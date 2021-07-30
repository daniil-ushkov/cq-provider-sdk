package providertest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v4"

	"github.com/cloudquery/cq-provider-sdk/cqproto"
	"github.com/cloudquery/cq-provider-sdk/logging"
	"github.com/cloudquery/cq-provider-sdk/provider"
	"github.com/cloudquery/cq-provider-sdk/provider/schema"
	"github.com/cloudquery/faker/v3"
	"github.com/georgysavva/scany/pgxscan"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tmccombs/hcl2json/convert"
)

type ResourceTestData struct {
	Table          *schema.Table
	Config         interface{}
	Resources      []string
	Configure      func(logger hclog.Logger, data interface{}) (schema.ClientMeta, error)
	SkipEmptyJsonB bool
	AtLeastOne     map[string][][]string
	Optional       map[string][]string
}

func TestResource(t *testing.T, providerCreator func() *provider.Provider, resource ResourceTestData) {
	if err := faker.SetRandomMapAndSliceMinSize(1); err != nil {
		t.Fatal(err)
	}
	if err := faker.SetRandomMapAndSliceMaxSize(1); err != nil {
		t.Fatal(err)
	}
	conn, err := setupDatabase()
	if err != nil {
		t.Fatal(err)
	}
	// Write configuration as a block and extract it out passing that specific block data as part of the configure provider
	f := hclwrite.NewFile()
	f.Body().AppendBlock(gohcl.EncodeAsBlock(resource.Config, "configuration"))
	data, err := convert.Bytes(f.Bytes(), "config.json", convert.Options{})
	require.Nil(t, err)
	hack := map[string]interface{}{}
	require.Nil(t, json.Unmarshal(data, &hack))
	data, err = json.Marshal(hack["configuration"].([]interface{})[0])
	require.Nil(t, err)

	testProvider := providerCreator()
	testProvider.Logger = logging.New(hclog.DefaultOptions)
	testProvider.Configure = resource.Configure
	_, err = testProvider.ConfigureProvider(context.Background(), &cqproto.ConfigureProviderRequest{
		CloudQueryVersion: "",
		Connection: cqproto.ConnectionDetails{DSN: getEnv("DATABASE_URL",
			"host=localhost user=postgres password=pass DB.name=postgres port=5432")},
		Config: data,
	})
	assert.Nil(t, err)

	err = testProvider.FetchResources(context.Background(), &cqproto.FetchResourcesRequest{Resources: []string{findResourceFromTableName(resource.Table, testProvider.ResourceMap)}}, fakeResourceSender{})
	assert.Nil(t, err)
	verifyAtLeastOne(t, resource, conn)
	verifyOptionalQuery(t, resource, conn)
}

func findResourceFromTableName(table *schema.Table, tables map[string]*schema.Table) string {
	for resource, t := range tables {
		if table.Name == t.Name {
			return resource
		}
	}
	return ""
}

type fakeResourceSender struct{}

func (f fakeResourceSender) Send(r *cqproto.FetchResourcesResponse) error {
	if r.Error != "" {
		fmt.Printf(r.Error)
	}
	return nil
}

func setupDatabase() (*pgx.Conn, error) {
	dbCfg, err := pgx.ParseConfig(getEnv("DATABASE_URL",
		"host=localhost user=postgres password=pass DB.name=postgres port=5432"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse config. %w", err)
	}
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, dbCfg)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to database. %w", err)
	}
	return conn, nil

}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

var selectColumnsIsNull, _ = NewQueryTemplate(
	`select * from {{.Table}} where {{.Columns | arrprintf "%v is null" | join " and "}};`,
)

func verifyAtLeastOne(t *testing.T, tc ResourceTestData, conn pgxscan.Querier) {
	for _, table := range getTablesFromMainTable(tc.Table) {
		for _, columns := range tc.AtLeastOne[table.Name] {
			rows, err := selectColumnsIsNull.Query(conn, map[string]interface{}{"Table": table.Name, "Columns": columns})
			if err != nil {
				t.Fatal(err)
			}
			// if response is not empty
			if rows.Next() {
				t.Fatal("oneof test failed")
			}
		}
	}
}

func verifyOptionalQuery(t *testing.T, tc ResourceTestData, conn pgxscan.Querier) {
	for _, table := range getTablesFromMainTable(tc.Table) {
		columnsMap := map[string]struct{}{}
		for _, column := range table.Columns {
			columnsMap[column.Name] = struct{}{}
		}

		for _, column := range tc.Optional[table.Name] {
			delete(columnsMap, column)
		}

		for _, columns := range tc.AtLeastOne[table.Name] {
			for _, column := range columns {
				delete(columnsMap, column)
			}
		}

		columns := make([]string, 0, len(columnsMap))
		for k := range columnsMap {
			columns = append(columns, k)
		}

		rows, err := selectColumnsIsNull.Query(conn, map[string]interface{}{"Table": table.Name, "Columns": columns})
		if err != nil {
			t.Fatal(err)
		}
		// if response is not empty
		if rows.Next() {
			t.Fatal("optional test failed")
		}
	}
}

func getTablesFromMainTable(table *schema.Table) []*schema.Table {
	var res []*schema.Table
	res = append(res, table)
	for _, t := range table.Relations {
		res = append(res, getTablesFromMainTable(t)...)
	}
	return res
}
