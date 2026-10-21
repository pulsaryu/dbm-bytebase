package v1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bytebase/bytebase/backend/common"
	"github.com/bytebase/bytebase/backend/common/testcontainer"
	"github.com/bytebase/bytebase/backend/enterprise"
	storepb "github.com/bytebase/bytebase/backend/generated-go/store"
	v1pb "github.com/bytebase/bytebase/backend/generated-go/v1"
	parserbase "github.com/bytebase/bytebase/backend/plugin/parser/base"
	"github.com/bytebase/bytebase/backend/store"
)

func instanceOn(engine storepb.Engine) *store.InstanceMessage {
	return &store.InstanceMessage{
		ResourceID: "inst",
		Metadata:   &storepb.Instance{Engine: engine},
	}
}

func unresolvedSpan() *parserbase.QuerySpan {
	return &parserbase.QuerySpan{
		Type: parserbase.Select,
		UnresolvedColumnsError: &parserbase.UnresolvedColumnsError{
			Relations: []parserbase.ColumnResource{
				{Database: "db", Schema: "public", Table: "t"},
			},
		},
	}
}

func TestMaskingBlockedByUnresolvedColumns(t *testing.T) {
	testCases := []struct {
		name     string
		span     *parserbase.QuerySpan
		instance *store.InstanceMessage
		want     bool
	}{
		{
			name:     "unresolved columns on a masking engine blocks",
			span:     unresolvedSpan(),
			instance: instanceOn(storepb.Engine_POSTGRES),
			want:     true,
		},
		{
			// CockroachDB shares the PostgreSQL extractor but does not support masking.
			name:     "engine that never masks does not block",
			span:     unresolvedSpan(),
			instance: instanceOn(storepb.Engine_COCKROACHDB),
			want:     false,
		},
		{
			name:     "resolved span does not block",
			span:     &parserbase.QuerySpan{Type: parserbase.Select},
			instance: instanceOn(storepb.Engine_POSTGRES),
			want:     false,
		},
		{
			name:     "nil span does not block",
			span:     nil,
			instance: instanceOn(storepb.Engine_POSTGRES),
			want:     false,
		},
		{
			name:     "nil instance does not block",
			span:     unresolvedSpan(),
			instance: nil,
			want:     false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, maskingBlockedByUnresolvedColumns(tc.span, tc.instance))
		})
	}
}

// These engines must enforce the signal once their extractors produce it.
func TestMaskingEnginesAgreeWithSignalProducers(t *testing.T) {
	for _, engine := range []storepb.Engine{
		storepb.Engine_MYSQL,
		storepb.Engine_ORACLE,
		storepb.Engine_MSSQL,
		storepb.Engine_REDSHIFT,
	} {
		require.True(t, maskingBlockedByUnresolvedColumns(unresolvedSpan(), instanceOn(engine)),
			"%s masks, so it must act on the signal once its extractor sets one", engine)
	}
}

// notFoundSpan is the shape a MySQL view over a base table the data source account
// cannot read produces: the extractor resolves no result column and reports the base
// column it could not find.
func notFoundSpan() *parserbase.QuerySpan {
	database, table, column := "pgsmtlk", "pg_smtlk_sms", "receiver"
	return &parserbase.QuerySpan{
		Type:    parserbase.Select,
		Results: []parserbase.QuerySpanResult{},
		NotFoundError: &parserbase.ResourceNotFoundError{
			Database: &database,
			Table:    &table,
			Column:   &column,
		},
	}
}

func TestPreMaskingRefusal(t *testing.T) {
	okResult := []*v1pb.QueryResult{{Statement: "SELECT phone_prefix5 FROM vw_pg_smtlk_sms"}}
	errorResult := []*v1pb.QueryResult{{Statement: "SELECT phone_prefix5 FROM vw_pg_smtlk_sms", Error: "syntax error"}}
	unmaskableFunction := []*parserbase.QuerySpan{{
		Type:                      parserbase.Select,
		FunctionNotSupportedError: &parserbase.FunctionNotSupportedError{Function: "crypt"},
	}}

	testCases := []struct {
		name            string
		spans           []*parserbase.QuerySpan
		results         []*v1pb.QueryResult
		maskingEnabled  bool
		allowUnresolved bool
		want            bool
	}{
		{
			name:           "a source column missing from the synced schema",
			spans:          []*parserbase.QuerySpan{notFoundSpan()},
			results:        okResult,
			maskingEnabled: true,
			want:           true,
		},
		{
			name:            "the same statement under the policy that allows it",
			spans:           []*parserbase.QuerySpan{notFoundSpan()},
			results:         okResult,
			maskingEnabled:  true,
			allowUnresolved: true,
			want:            false,
		},
		{
			// Nothing is masked, so there is nothing for an unmaskable column to leak.
			name:    "masking off",
			spans:   []*parserbase.QuerySpan{notFoundSpan()},
			results: okResult,
			want:    false,
		},
		{
			name:            "a function masking cannot trace refuses either way",
			spans:           unmaskableFunction,
			results:         okResult,
			maskingEnabled:  true,
			allowUnresolved: true,
			want:            true,
		},
		{
			name:           "a statement the database rejected",
			spans:          []*parserbase.QuerySpan{notFoundSpan()},
			results:        errorResult,
			maskingEnabled: true,
			want:           false,
		},
		{
			name:           "more results than spans",
			spans:          nil,
			results:        okResult,
			maskingEnabled: true,
			want:           false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := preMaskingRefusal(tc.spans, tc.results, tc.maskingEnabled, tc.allowUnresolved)
			require.Equal(t, tc.want, got != nil, "got %v", got)
		})
	}
}

func TestUnresolvedMaskingLineage(t *testing.T) {
	testCases := []struct {
		name          string
		span          *parserbase.QuerySpan
		instance      *store.InstanceMessage
		resultErrored bool
		want          bool
	}{
		{
			name:     "a source column missing from the synced schema",
			span:     notFoundSpan(),
			instance: instanceOn(storepb.Engine_MYSQL),
			want:     true,
		},
		{
			// The rows that accompany the error are no more maskable, but the span
			// reports nothing beyond a lookup that failed on the way to them.
			name:          "a failed lookup on an errored result",
			span:          notFoundSpan(),
			instance:      instanceOn(storepb.Engine_MYSQL),
			resultErrored: true,
			want:          false,
		},
		{
			name:          "a relation with no columns, even on an errored result",
			span:          unresolvedSpan(),
			instance:      instanceOn(storepb.Engine_POSTGRES),
			resultErrored: true,
			want:          true,
		},
		{
			name:     "a fully resolved span",
			span:     &parserbase.QuerySpan{Type: parserbase.Select},
			instance: instanceOn(storepb.Engine_MYSQL),
			want:     false,
		},
		{
			name:     "no span",
			instance: instanceOn(storepb.Engine_MYSQL),
			want:     false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := unresolvedMaskingLineage(tc.span, tc.instance, tc.resultErrored)
			require.Equal(t, tc.want, got != nil, "got %v", got)
		})
	}
}

// TestMaskResultsAllowsUnresolvedColumns covers QueryDataPolicy's
// allow_unresolved_masking_columns: masking refuses rows it could not analyze unless
// the policy trades that refusal for availability, which a data source account with
// no privilege on a queried view's base table needs.
func TestMaskResultsAllowsUnresolvedColumns(t *testing.T) {
	t.Parallel()
	ctx := context.WithValue(context.Background(), common.WorkspaceIDContextKey, "default")
	db, stores, _ := testcontainer.NewMetadataDB(t)
	_, err := db.ExecContext(ctx, "INSERT INTO workspace (resource_id) VALUES ('default')")
	require.NoError(t, err)

	masker := NewQueryResultMasker(stores)
	instance := instanceOn(storepb.Engine_MYSQL)
	user := &store.UserMessage{Email: "caller@example.com"}
	const prefix = "+8613***"
	newResults := func() []*v1pb.QueryResult {
		return []*v1pb.QueryResult{{
			Statement:   "SELECT phone_prefix5 FROM vw_pg_smtlk_sms",
			ColumnNames: []string{"phone_prefix5"},
			Rows: []*v1pb.QueryRow{{Values: []*v1pb.RowValue{
				{Kind: &v1pb.RowValue_StringValue{StringValue: prefix}},
			}}},
		}}
	}

	spans := []*parserbase.QuerySpan{notFoundSpan()}

	refused := newResults()
	err = masker.MaskResults(ctx, spans, refused, instance, user, false)
	require.ErrorContains(t, err, "masking cannot be applied")
	require.ErrorContains(t, err, "column: receiver")

	allowed := newResults()
	require.NoError(t, masker.MaskResults(ctx, spans, allowed, instance, user, true))
	require.Equal(t, prefix, allowed[0].Rows[0].Values[0].GetStringValue())
}

func TestAllowUnresolvedMaskingColumnsFromEnv(t *testing.T) {
	testCases := []struct {
		value string
		want  bool
	}{
		{value: "", want: false},
		{value: "true", want: true},
		{value: "TRUE", want: true},
		{value: "1", want: true},
		{value: "false", want: false},
		{value: "0", want: false},
		// Not a value strconv.ParseBool takes, so it is not an opt-in.
		{value: "yes", want: false},
	}
	for _, tc := range testCases {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(allowUnresolvedMaskingColumnsEnv, tc.value)
			require.Equal(t, tc.want, allowUnresolvedMaskingColumnsFromEnv())
		})
	}
}

// TestGetEffectiveQueryDataPolicyUnresolvedMaskingColumns covers the two ways the
// switch can be on. The environment one has to reach an operator who cannot set the
// policy, so it holds without the query-policy license the policy itself sits behind.
func TestGetEffectiveQueryDataPolicyUnresolvedMaskingColumns(t *testing.T) {
	ctx := context.WithValue(context.Background(), common.WorkspaceIDContextKey, "default")
	db, stores, _ := testcontainer.NewMetadataDB(t)
	_, err := db.ExecContext(ctx, "INSERT INTO workspace (resource_id) VALUES ('default')")
	require.NoError(t, err)
	// Release mode prod with no license: FEATURE_QUERY_POLICY is off, so the policy
	// is never read.
	unlicensed, err := enterprise.NewLicenseService(common.ReleaseModeProd, stores, false, "")
	require.NoError(t, err)

	t.Run("off by default", func(t *testing.T) {
		require.False(t, getEffectiveQueryDataPolicy(ctx, stores, unlicensed, 0, "test-project").AllowUnresolvedMaskingColumns)
	})

	t.Run("on from the environment, unlicensed", func(t *testing.T) {
		t.Setenv(allowUnresolvedMaskingColumnsEnv, "true")
		require.True(t, getEffectiveQueryDataPolicy(ctx, stores, unlicensed, 0, "test-project").AllowUnresolvedMaskingColumns)
	})
}
