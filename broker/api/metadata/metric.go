package metadata

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/lindb/lindb/broker/api"
	"github.com/lindb/lindb/constants"
	"github.com/lindb/lindb/coordinator/broker"
	"github.com/lindb/lindb/coordinator/replica"
	"github.com/lindb/lindb/parallel"
	"github.com/lindb/lindb/sql/stmt"
)

// MetricAPI represents the metric metadata suggest api
type MetricAPI struct {
	replicaStateMachine replica.StatusStateMachine
	nodeStateMachine    broker.NodeStateMachine
	executorFactory     parallel.ExecutorFactory
	jobManager          parallel.JobManager
}

// NewMetricAPI creates the suggest api
func NewMetricAPI(replicaStateMachine replica.StatusStateMachine, nodeStateMachine broker.NodeStateMachine,
	executorFactory parallel.ExecutorFactory, jobManager parallel.JobManager,
) *MetricAPI {
	return &MetricAPI{
		replicaStateMachine: replicaStateMachine,
		nodeStateMachine:    nodeStateMachine,
		executorFactory:     executorFactory,
		jobManager:          jobManager,
	}
}

// SuggestMetrics suggests metric names based on prefix
func (m *MetricAPI) SuggestMetrics(w http.ResponseWriter, r *http.Request) {
	db, namespace, metricNamePrefix, limit, err := getCommonParams(r)
	if err != nil {
		api.Error(w, err)
		return
	}
	m.suggest(w, db, namespace, &stmt.Metadata{
		Type:       stmt.Metric,
		MetricName: metricNamePrefix,
		Limit:      limit,
	})
}

// SuggestTagKeys suggests tag keys based on prefix
func (m *MetricAPI) SuggestTagKeys(w http.ResponseWriter, r *http.Request) {
	db, namespace, tagKeyPrefix, limit, err := getCommonParams(r)
	if err != nil {
		api.Error(w, err)
		return
	}
	metricName, err := api.GetParamsFromRequest("metric", r, "", true)
	if err != nil {
		api.Error(w, err)
		return
	}
	m.suggest(w, db, namespace, &stmt.Metadata{
		Type:       stmt.TagKey,
		MetricName: metricName,
		TagKey:     tagKeyPrefix,
		Limit:      limit,
	})
}

// SuggestTagValues suggests tag values based on prefix
func (m *MetricAPI) SuggestTagValues(w http.ResponseWriter, r *http.Request) {
	db, namespace, tagValuePrefix, limit, err := getCommonParams(r)
	if err != nil {
		api.Error(w, err)
		return
	}
	metricName, err := api.GetParamsFromRequest("metric", r, "", true)
	if err != nil {
		api.Error(w, err)
		return
	}
	tagKey, err := api.GetParamsFromRequest("tagKey", r, "", true)
	if err != nil {
		api.Error(w, err)
		return
	}
	m.suggest(w, db, namespace, &stmt.Metadata{
		Type:       stmt.TagValue,
		MetricName: metricName,
		TagKey:     tagKey,
		TagValue:   tagValuePrefix,
		Limit:      limit,
	})
}

// suggest executes the suggest query
func (m *MetricAPI) suggest(w http.ResponseWriter, database string, namespace string, request *stmt.Metadata) {
	//TODO add timeout cfg
	ctx, cancel := context.WithTimeout(context.TODO(), time.Minute)
	defer cancel()

	exec := m.executorFactory.NewMetadataBrokerExecutor(ctx, database, namespace, request, m.replicaStateMachine, m.nodeStateMachine, m.jobManager)
	values, err := exec.Execute()
	if err != nil {
		api.Error(w, err)
		return
	}
	api.OK(w, values)
}

// getCommonParams gets the common params from http request
func getCommonParams(r *http.Request) (db, namespace, prefix string, limit int, err error) {
	db, err = api.GetParamsFromRequest("db", r, "", true)
	if err != nil {
		return
	}
	namespace, _ = api.GetParamsFromRequest("ns", r, constants.DefaultNamespace, false)
	prefix, _ = api.GetParamsFromRequest("prefix", r, "", false)
	limitStr, _ := api.GetParamsFromRequest("limit", r, "100", false)
	l, err := strconv.ParseInt(limitStr, 10, 64)
	if err != nil {
		return
	}
	limit = int(l)
	return
}
