package providertest

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"

	"github.com/jackc/pgx/v4"

	"github.com/georgysavva/scany/pgxscan"
)

type QueryTemplate struct {
	template.Template
}

var tmplFunctions = template.FuncMap{
	"join": func(sep string, elems []string) string { return strings.Join(elems, sep) },
	"arrprintf": func(format string, elems []string) []string {
		for i := range elems {
			elems[i] = fmt.Sprintf(format, elems[i])
		}
		return elems
	},
}

func NewQueryTemplate(queryTmpl string) (*QueryTemplate, error) {
	tmpl, err := template.New("queryTmpl").Funcs(tmplFunctions).Parse(queryTmpl)
	return &QueryTemplate{*tmpl}, err
}

func (t *QueryTemplate) Query(conn pgxscan.Querier, data interface{}) (pgx.Rows, error) {
	var buf bytes.Buffer
	err := t.Execute(&buf, data)
	if err != nil {
		return nil, err
	}
	query := buf.String()
	return conn.Query(context.Background(), query)
}
