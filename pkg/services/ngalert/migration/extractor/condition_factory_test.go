package extractor

import (
	"testing"

	"github.com/grafana/grafana/pkg/infra/db/dbtest"
	"github.com/stretchr/testify/require"
)

func TestQueryCondition(t *testing.T) {
	setup := func() *queryConditionTestContext {
		ctx := &queryConditionTestContext{}
		db := dbtest.NewFakeDB()
		ctx.reducer = `{"type":"avg"}`
		ctx.evaluator = `{"type":"gt","params":[100]}`
		return ctx
	}

	t.Run("Can read query condition from json model", func(t *testing.T) {
		ctx := setup()
		_, err := ctx.exec(t)
		require.Nil(t, err)

		require.Equal(t, "5m", ctx.condition.Query.From)
		require.Equal(t, "now", ctx.condition.Query.To)
		require.Equal(t, int64(1), ctx.condition.Query.DatasourceID)

		t.Run("Can read query reducer", func(t *testing.T) {
			reducer := ctx.condition.Reducer
			require.Equal(t, "avg", reducer.Type)
		})

		t.Run("Can read evaluator", func(t *testing.T) {
			evaluator, ok := ctx.condition.Evaluator.(*thresholdEvaluator)
			require.True(t, ok)
			require.Equal(t, "gt", evaluator.Type)
		})
	})
}

type queryConditionTestContext struct {
	reducer   string
	evaluator string
	condition *QueryCondition
}
