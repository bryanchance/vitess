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

package vtctld

import (
	"context"
	"net/http"
	"strings"

	"github.com/spf13/pflag"

	"vitess.io/vitess/go/vt/sqlparser"

	"vitess.io/vitess/go/mysql/collations"

	"vitess.io/vitess/go/acl"
	"vitess.io/vitess/go/vt/logutil"
	"vitess.io/vitess/go/vt/servenv"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/topo/topoproto"
	"vitess.io/vitess/go/vt/vttablet/tmclient"
	"vitess.io/vitess/go/vt/wrangler"

	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
)

var (
	actionTimeout = wrangler.DefaultActionTimeout
)

// ActionResult contains the result of an action. If Error, the action failed.
type ActionResult struct {
	Name       string
	Parameters string
	Output     string
	Error      bool
}

func (ar *ActionResult) error(text string) {
	ar.Error = true
	ar.Output = text
}

func init() {
	for _, cmd := range []string{"vtcombo", "vtctld"} {
		servenv.OnParseFor(cmd, registerActionRepositoryFlags)
	}
}

func registerActionRepositoryFlags(fs *pflag.FlagSet) {
	fs.DurationVar(&actionTimeout, "action_timeout", actionTimeout, "time to wait for an action before resorting to force")
}

// action{Keyspace,Shard,Tablet}Method is a function that performs
// some action on a Topology object. It should return a message for
// the user or an empty string in case there's nothing interesting to
// be communicated.
type actionKeyspaceMethod func(ctx context.Context, wr *wrangler.Wrangler, keyspace string) (output string, err error)

type actionShardMethod func(ctx context.Context, wr *wrangler.Wrangler, keyspace, shard string) (output string, err error)

type actionTabletMethod func(ctx context.Context, wr *wrangler.Wrangler, tabletAlias *topodatapb.TabletAlias) (output string, err error)

type actionTabletRecord struct {
	role   string
	method actionTabletMethod
}

// ActionRepository is a repository of actions that can be performed
// on a {Keyspace,Shard,Tablet}.
type ActionRepository struct {
	keyspaceActions map[string]actionKeyspaceMethod
	shardActions    map[string]actionShardMethod
	tabletActions   map[string]actionTabletRecord
	ts              *topo.Server
	collationEnv    *collations.Environment
	parser          *sqlparser.Parser
}

// NewActionRepository creates and returns a new ActionRepository,
// with no actions.
func NewActionRepository(ts *topo.Server, collationEnv *collations.Environment, parser *sqlparser.Parser) *ActionRepository {
	return &ActionRepository{
		keyspaceActions: make(map[string]actionKeyspaceMethod),
		shardActions:    make(map[string]actionShardMethod),
		tabletActions:   make(map[string]actionTabletRecord),
		ts:              ts,
		collationEnv:    collationEnv,
		parser:          parser,
	}
}

// RegisterKeyspaceAction registers a new action on a keyspace.
func (ar *ActionRepository) RegisterKeyspaceAction(name string, method actionKeyspaceMethod) {
	ar.keyspaceActions[name] = method
}

// RegisterShardAction registers a new action on a shard.
func (ar *ActionRepository) RegisterShardAction(name string, method actionShardMethod) {
	ar.shardActions[name] = method
}

// RegisterTabletAction registers a new action on a tablet.
func (ar *ActionRepository) RegisterTabletAction(name, role string, method actionTabletMethod) {
	ar.tabletActions[name] = actionTabletRecord{
		role:   role,
		method: method,
	}
}

// ApplyKeyspaceAction applies the provided action to the keyspace.
func (ar *ActionRepository) ApplyKeyspaceAction(ctx context.Context, actionName, keyspace string) *ActionResult {
	result := &ActionResult{Name: actionName, Parameters: keyspace}

	action, ok := ar.keyspaceActions[actionName]
	if !ok {
		result.error("Unknown keyspace action")
		return result
	}

	ctx, cancel := context.WithTimeout(ctx, actionTimeout)
	wr := wrangler.New(logutil.NewConsoleLogger(), ar.ts, tmclient.NewTabletManagerClient(), ar.collationEnv, ar.parser)
	output, err := action(ctx, wr, keyspace)
	cancel()
	if err != nil {
		result.error(err.Error())
		return result
	}
	result.Output = output
	return result
}

// ApplyShardAction applies the provided action to the shard.
func (ar *ActionRepository) ApplyShardAction(ctx context.Context, actionName, keyspace, shard string) *ActionResult {
	// if the shard name contains a '-', we assume it's the
	// name for a ranged based shard, so we lower case it.
	if strings.Contains(shard, "-") {
		shard = strings.ToLower(shard)
	}
	result := &ActionResult{Name: actionName, Parameters: keyspace + "/" + shard}

	action, ok := ar.shardActions[actionName]
	if !ok {
		result.error("Unknown shard action")
		return result
	}

	ctx, cancel := context.WithTimeout(ctx, actionTimeout)
	wr := wrangler.New(logutil.NewConsoleLogger(), ar.ts, tmclient.NewTabletManagerClient(), ar.collationEnv, ar.parser)
	output, err := action(ctx, wr, keyspace, shard)
	cancel()
	if err != nil {
		result.error(err.Error())
		return result
	}
	result.Output = output
	return result
}

// ApplyTabletAction applies the provided action to the tablet.
func (ar *ActionRepository) ApplyTabletAction(ctx context.Context, actionName string, tabletAlias *topodatapb.TabletAlias, r *http.Request) *ActionResult {
	result := &ActionResult{
		Name:       actionName,
		Parameters: topoproto.TabletAliasString(tabletAlias),
	}

	action, ok := ar.tabletActions[actionName]
	if !ok {
		result.error("Unknown tablet action")
		return result
	}

	// check the role
	if action.role != "" {
		if err := acl.CheckAccessHTTP(r, action.role); err != nil {
			result.error("Access denied")
			return result
		}
	}

	// run the action
	ctx, cancel := context.WithTimeout(ctx, actionTimeout)
	wr := wrangler.New(logutil.NewConsoleLogger(), ar.ts, tmclient.NewTabletManagerClient(), ar.collationEnv, ar.parser)
	output, err := action.method(ctx, wr, tabletAlias)
	cancel()
	if err != nil {
		result.error(err.Error())
		return result
	}
	result.Output = output
	return result
}
