/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vtgate

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/google/safehtml/template"

	"vitess.io/vitess/go/acl"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/logz"
	"vitess.io/vitess/go/vt/vtgate/engine"
)

var (
	queryzHeader = []byte(`<thead>
		<tr>
			<th>Query</th>
			<th>Count</th>
			<th>Time</th>
			<th>Shard Queries</th>
			<th>RowsAffected</th>
			<th>RowsReturned</th>
			<th>Errors</th>
			<th>Time per query</th>
			<th>Shard queries per query</th>
			<th>RowsAffected per query</th>
			<th>RowsReturned per query</th>
			<th>Errors per query</th>
		</tr>
        </thead>
	`)
	queryzTmpl = template.Must(template.New("example").Parse(`
		<tr class="{{.Color}}">
			<td>{{.Query}}</td>
			<td>{{.Count}}</td>
			<td>{{.Time}}</td>
			<td>{{.ShardQueries}}</td>
			<td>{{.RowsAffected}}</td>
			<td>{{.RowsReturned}}</td>
			<td>{{.Errors}}</td>
			<td>{{.TimePQ}}</td>
			<td>{{.ShardQueriesPQ}}</td>
			<td>{{.RowsAffectedPQ}}</td>
			<td>{{.RowsReturnedPQ}}</td>
			<td>{{.ErrorsPQ}}</td>
		</tr>
	`))
)

// queryzRow is used for rendering query stats
// using go's template.
type queryzRow struct {
	Query        string
	Table        string
	Count        uint64
	tm           time.Duration
	ShardQueries uint64
	RowsAffected uint64
	RowsReturned uint64
	Errors       uint64
	Color        string
}

// Time returns the total time as a string.
func (qzs *queryzRow) Time() string {
	return fmt.Sprintf("%.6f", float64(qzs.tm)/1e9)
}

func (qzs *queryzRow) timePQ() float64 {
	return float64(qzs.tm) / (1e9 * float64(qzs.Count))
}

// TimePQ returns the time per query as a string.
func (qzs *queryzRow) TimePQ() string {
	return fmt.Sprintf("%.6f", qzs.timePQ())
}

// ShardQueriesPQ returns the shard query count per query as a string.
func (qzs *queryzRow) ShardQueriesPQ() string {
	val := float64(qzs.ShardQueries) / float64(qzs.Count)
	return fmt.Sprintf("%.6f", val)
}

// RowsAffectedPQ returns the row affected per query as a string.
func (qzs *queryzRow) RowsAffectedPQ() string {
	val := float64(qzs.RowsAffected) / float64(qzs.Count)
	return fmt.Sprintf("%.6f", val)
}

// RowsReturnedPQ returns the row returned per query as a string.
func (qzs *queryzRow) RowsReturnedPQ() string {
	val := float64(qzs.RowsReturned) / float64(qzs.Count)
	return fmt.Sprintf("%.6f", val)
}

// ErrorsPQ returns the error count per query as a string.
func (qzs *queryzRow) ErrorsPQ() string {
	return fmt.Sprintf("%.6f", float64(qzs.Errors)/float64(qzs.Count))
}

type queryzSorter struct {
	rows []*queryzRow
	less func(row1, row2 *queryzRow) bool
}

func (s *queryzSorter) Len() int           { return len(s.rows) }
func (s *queryzSorter) Swap(i, j int)      { s.rows[i], s.rows[j] = s.rows[j], s.rows[i] }
func (s *queryzSorter) Less(i, j int) bool { return s.less(s.rows[i], s.rows[j]) }

func queryzHandler(e *Executor, w http.ResponseWriter, r *http.Request) {
	if err := acl.CheckAccessHTTP(r, acl.DEBUGGING); err != nil {
		acl.SendError(w, err)
		return
	}
	logz.StartHTMLTable(w)
	defer logz.EndHTMLTable(w)
	w.Write(queryzHeader)

	sorter := queryzSorter{
		rows: nil,
		less: func(row1, row2 *queryzRow) bool {
			return row1.timePQ() > row2.timePQ()
		},
	}

	e.ForEachPlan(func(plan *engine.Plan) bool {
		Value := &queryzRow{
			Query: logz.Wrappable(e.parser.TruncateForUI(plan.Original)),
		}
		Value.Count, Value.tm, Value.ShardQueries, Value.RowsAffected, Value.RowsReturned, Value.Errors = plan.Stats()
		var timepq time.Duration
		if Value.Count != 0 {
			timepq = time.Duration(uint64(Value.tm) / Value.Count)
		}
		if timepq < 10*time.Millisecond {
			Value.Color = "low"
		} else if timepq < 100*time.Millisecond {
			Value.Color = "medium"
		} else {
			Value.Color = "high"
		}
		sorter.rows = append(sorter.rows, Value)
		return true
	})

	sort.Sort(&sorter)
	for _, row := range sorter.rows {
		if err := queryzTmpl.Execute(w, row); err != nil {
			log.Errorf("queryz: couldn't execute template: %v", err)
		}
	}
}
