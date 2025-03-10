// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package promql

import (
	"bytes"
	"container/heap"
	"context"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/uber/jaeger-client-go"

	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/timestamp"
	"github.com/prometheus/prometheus/pkg/value"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/stats"

	"github.com/timescale/promscale/pkg/util"
)

// A Queryable handles queries against a storage.
// Use it when you need to have access to all samples without chunk encoding abstraction e.g promQL.
type Queryable interface {
	// Querier returns a new Querier on the storage.
	Querier(ctx context.Context, mint, maxt int64) (Querier, error)
}

// Querier provides querying access over time series data of a fixed time range.
type Querier interface {
	// LabelValues returns all potential values for a label name.
	// It is not safe to use the strings beyond the lifefime of the querier.
	LabelValues(name string) ([]string, storage.Warnings, error)

	// LabelNames returns all the unique label names present in the block in sorted order.
	LabelNames() ([]string, storage.Warnings, error)

	// Close releases the resources of the Querier.
	Close() error

	// Select returns a set of series that matches the given label matchers.
	// Caller can specify if it requires returned series to be sorted. Prefer not requiring sorting for better performance.
	// It allows passing hints that can help in optimising select, but it's up to implementation how this is used if used at all.
	Select(sortSeries bool, hints *storage.SelectHints, nodes []parser.Node, matchers ...*labels.Matcher) (storage.SeriesSet, parser.Node)
}

const (
	namespace            = util.PromNamespace
	subsystem            = "engine"
	queryTag             = "query"
	env                  = "query execution"
	defaultLookbackDelta = 5 * time.Minute

	// The largest SampleValue that can be converted to an int64 without overflow.
	maxInt64 = 9223372036854774784
	// The smallest SampleValue that can be converted to an int64 without underflow.
	minInt64 = -9223372036854775808
)

type engineMetrics struct {
	currentQueries       prometheus.Gauge
	maxConcurrentQueries prometheus.Gauge
	queryLogEnabled      prometheus.Gauge
	queryLogFailures     prometheus.Counter
	queryQueueTime       prometheus.Observer
	queryPrepareTime     prometheus.Observer
	queryInnerEval       prometheus.Observer
	queryResultSort      prometheus.Observer
}

// convertibleToInt64 returns true if v does not over-/underflow an int64.
func convertibleToInt64(v float64) bool {
	return v <= maxInt64 && v >= minInt64
}

type (
	// ErrQueryTimeout is returned if a query timed out during processing.
	ErrQueryTimeout string
	// ErrQueryCanceled is returned if a query was canceled during processing.
	ErrQueryCanceled string
	// ErrTooManySamples is returned if a query would load more than the maximum allowed samples into memory.
	ErrTooManySamples string
	// ErrStorage is returned if an error was encountered in the storage layer
	// during query handling.
	ErrStorage struct{ Err error }
)

func (e ErrQueryTimeout) Error() string {
	return fmt.Sprintf("query timed out in %s", string(e))
}
func (e ErrQueryCanceled) Error() string {
	return fmt.Sprintf("query was canceled in %s", string(e))
}
func (e ErrTooManySamples) Error() string {
	return fmt.Sprintf("query processing would load too many samples into memory in %s", string(e))
}
func (e ErrStorage) Error() string {
	return e.Err.Error()
}

// QueryLogger is an interface that can be used to log all the queries logged
// by the engine.
type QueryLogger interface {
	Log(...interface{}) error
	Close() error
}

// A Query is derived from an a raw query string and can be run against an engine
// it is associated with.
type Query interface {
	// Exec processes the query. Can only be called once.
	Exec(ctx context.Context) *Result
	// Close recovers memory used by the query result.
	Close()
	// Statement returns the parsed statement of the query.
	Statement() parser.Statement
	// Stats returns statistics about the lifetime of the query.
	Stats() *stats.QueryTimers
	// Cancel signals that a running query execution should be aborted.
	Cancel()
}

// query implements the Query interface.
type query struct {
	// Underlying data provider.
	queryable Queryable
	// The original query string.
	q string
	// Statement of the parsed query.
	stmt parser.Statement
	// Timer stats for the query execution.
	stats *stats.QueryTimers
	// Result matrix for reuse.
	matrix Matrix
	// Cancellation function for the query.
	cancel func()

	// The engine against which the query is executed.
	ng *Engine
}

type queryOrigin struct{}

// Statement implements the Query interface.
func (q *query) Statement() parser.Statement {
	return q.stmt
}

// Stats implements the Query interface.
func (q *query) Stats() *stats.QueryTimers {
	return q.stats
}

// Cancel implements the Query interface.
func (q *query) Cancel() {
	if q.cancel != nil {
		q.cancel()
	}
}

// Close implements the Query interface.
func (q *query) Close() {
	for _, s := range q.matrix {
		putPointSlice(s.Points)
	}
}

// Exec implements the Query interface.
func (q *query) Exec(ctx context.Context) *Result {
	if span := opentracing.SpanFromContext(ctx); span != nil {
		span.SetTag(queryTag, q.stmt.String())
	}

	// Exec query.
	res, warnings, err := q.ng.exec(ctx, q)

	return &Result{Err: err, Value: res, Warnings: warnings}
}

// contextDone returns an error if the context was canceled or timed out.
func contextDone(ctx context.Context, env string) error {
	if err := ctx.Err(); err != nil {
		return contextErr(err, env)
	}
	return nil
}

func contextErr(err error, env string) error {
	switch err {
	case context.Canceled:
		return ErrQueryCanceled(env)
	case context.DeadlineExceeded:
		return ErrQueryTimeout(env)
	default:
		return err
	}
}

// EngineOpts contains configuration options used when creating a new Engine.
type EngineOpts struct {
	Logger             log.Logger
	Reg                prometheus.Registerer
	MaxSamples         int64
	Timeout            time.Duration
	ActiveQueryTracker *ActiveQueryTracker
	// LookbackDelta determines the time since the last sample after which a time
	// series is considered stale.
	LookbackDelta time.Duration

	// NoStepSubqueryIntervalFn is the default evaluation interval of
	// a subquery in milliseconds if no step in range vector was specified `[30m:<step>]`.
	NoStepSubqueryIntervalFn func(rangeMillis int64) int64

	// EnableAtModifier if true enables @ modifier. Disabled otherwise.
	EnableAtModifier bool
	// EnableNegativeOffset if true enables negative (-) offset values. Disabled otherwise.
	EnableNegativeOffset bool
}

// Engine handles the lifetime of queries from beginning to end.
// It is connected to a querier.
type Engine struct {
	logger                   log.Logger
	metrics                  *engineMetrics
	timeout                  time.Duration
	maxSamplesPerQuery       int64
	activeQueryTracker       *ActiveQueryTracker
	queryLogger              QueryLogger
	queryLoggerLock          sync.RWMutex
	lookbackDelta            time.Duration
	noStepSubqueryIntervalFn func(rangeMillis int64) int64
	enableAtModifier         bool
	enableNegativeOffset     bool
}

// NewEngine returns a new engine.
func NewEngine(opts EngineOpts) *Engine {
	if opts.Logger == nil {
		opts.Logger = log.NewNopLogger()
	}

	queryResultSummary := prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Namespace:  namespace,
		Subsystem:  subsystem,
		Name:       "query_duration_seconds",
		Help:       "Query timings",
		Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
	},
		[]string{"slice"},
	)

	metrics := &engineMetrics{
		currentQueries: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "queries",
			Help:      "The current number of queries being executed or waiting.",
		}),
		queryLogEnabled: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "query_log_enabled",
			Help:      "State of the query log.",
		}),
		queryLogFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "query_log_failures_total",
			Help:      "The number of query log failures.",
		}),
		maxConcurrentQueries: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "queries_concurrent_max",
			Help:      "The max number of concurrent queries.",
		}),
		queryQueueTime:   queryResultSummary.WithLabelValues("queue_time"),
		queryPrepareTime: queryResultSummary.WithLabelValues("prepare_time"),
		queryInnerEval:   queryResultSummary.WithLabelValues("inner_eval"),
		queryResultSort:  queryResultSummary.WithLabelValues("result_sort"),
	}

	if t := opts.ActiveQueryTracker; t != nil {
		metrics.maxConcurrentQueries.Set(float64(t.GetMaxConcurrent()))
	} else {
		metrics.maxConcurrentQueries.Set(-1)
	}

	if opts.LookbackDelta == 0 {
		opts.LookbackDelta = defaultLookbackDelta
		if l := opts.Logger; l != nil {
			level.Debug(l).Log("msg", "Lookback delta is zero, setting to default value", "value", defaultLookbackDelta)
		}
	}

	if opts.Reg != nil {
		opts.Reg.MustRegister(
			metrics.currentQueries,
			metrics.maxConcurrentQueries,
			metrics.queryLogEnabled,
			metrics.queryLogFailures,
			queryResultSummary,
		)
	}

	return &Engine{
		timeout:                  opts.Timeout,
		logger:                   opts.Logger,
		metrics:                  metrics,
		maxSamplesPerQuery:       opts.MaxSamples,
		activeQueryTracker:       opts.ActiveQueryTracker,
		lookbackDelta:            opts.LookbackDelta,
		noStepSubqueryIntervalFn: opts.NoStepSubqueryIntervalFn,
		enableAtModifier:         opts.EnableAtModifier,
		enableNegativeOffset:     opts.EnableNegativeOffset,
	}
}

// SetQueryLogger sets the query logger.
func (ng *Engine) SetQueryLogger(l QueryLogger) {
	ng.queryLoggerLock.Lock()
	defer ng.queryLoggerLock.Unlock()

	if ng.queryLogger != nil {
		// An error closing the old file descriptor should
		// not make reload fail; only log a warning.
		if err := ng.queryLogger.Close(); err != nil {
			level.Warn(ng.logger).Log("msg", "Error while closing the previous query log file", "err", err)
		}
	}

	ng.queryLogger = l

	if l != nil {
		ng.metrics.queryLogEnabled.Set(1)
	} else {
		ng.metrics.queryLogEnabled.Set(0)
	}
}

// NewInstantQuery returns an evaluation query for the given expression at the given time.
func (ng *Engine) NewInstantQuery(q Queryable, qs string, ts time.Time) (Query, error) {
	expr, err := parser.ParseExpr(qs)
	if err != nil {
		return nil, err
	}
	qry, err := ng.newQuery(q, expr, ts, ts, 0)
	if err != nil {
		return nil, err
	}
	qry.q = qs

	return qry, nil
}

// NewRangeQuery returns an evaluation query for the given time range and with
// the resolution set by the interval.
func (ng *Engine) NewRangeQuery(q Queryable, qs string, start, end time.Time, interval time.Duration) (Query, error) {
	expr, err := parser.ParseExpr(qs)
	if err != nil {
		return nil, err
	}
	if expr.Type() != parser.ValueTypeVector && expr.Type() != parser.ValueTypeScalar {
		return nil, errors.Errorf("invalid expression type %q for range query, must be Scalar or instant Vector", parser.DocumentedType(expr.Type()))
	}
	qry, err := ng.newQuery(q, expr, start, end, interval)
	if err != nil {
		return nil, err
	}
	qry.q = qs

	return qry, nil
}

func (ng *Engine) newQuery(q Queryable, expr parser.Expr, start, end time.Time, interval time.Duration) (*query, error) {
	if err := ng.validateOpts(expr); err != nil {
		return nil, err
	}

	es := &parser.EvalStmt{
		Expr:     PreprocessExpr(expr, start, end),
		Start:    start,
		End:      end,
		Interval: interval,
	}
	qry := &query{
		stmt:      es,
		ng:        ng,
		stats:     stats.NewQueryTimers(),
		queryable: q,
	}
	return qry, nil
}

var ErrValidationAtModifierDisabled = errors.New("@ modifier is disabled")
var ErrValidationNegativeOffsetDisabled = errors.New("negative offset is disabled")

func (ng *Engine) validateOpts(expr parser.Expr) error {
	if ng.enableAtModifier && ng.enableNegativeOffset {
		return nil
	}

	var atModifierUsed, negativeOffsetUsed bool

	var validationErr error
	parser.Inspect(expr, func(node parser.Node, path []parser.Node) error {
		switch n := node.(type) {
		case *parser.VectorSelector:
			if n.Timestamp != nil || n.StartOrEnd == parser.START || n.StartOrEnd == parser.END {
				atModifierUsed = true
			}
			if n.OriginalOffset < 0 {
				negativeOffsetUsed = true
			}

		case *parser.MatrixSelector:
			vs := n.VectorSelector.(*parser.VectorSelector)
			if vs.Timestamp != nil || vs.StartOrEnd == parser.START || vs.StartOrEnd == parser.END {
				atModifierUsed = true
			}
			if vs.OriginalOffset < 0 {
				negativeOffsetUsed = true
			}

		case *parser.SubqueryExpr:
			if n.Timestamp != nil || n.StartOrEnd == parser.START || n.StartOrEnd == parser.END {
				atModifierUsed = true
			}
			if n.OriginalOffset < 0 {
				negativeOffsetUsed = true
			}
		}

		if atModifierUsed && !ng.enableAtModifier {
			validationErr = ErrValidationAtModifierDisabled
			return validationErr
		}
		if negativeOffsetUsed && !ng.enableNegativeOffset {
			validationErr = ErrValidationNegativeOffsetDisabled
			return validationErr
		}

		return nil
	})

	return validationErr
}

func (ng *Engine) newTestQuery(f func(context.Context) error) Query {
	qry := &query{
		q:     "test statement",
		stmt:  parser.TestStmt(f),
		ng:    ng,
		stats: stats.NewQueryTimers(),
	}
	return qry
}

// exec executes the query.
//
// At this point per query only one EvalStmt is evaluated. Alert and record
// statements are not handled by the Engine.
func (ng *Engine) exec(ctx context.Context, q *query) (v parser.Value, ws storage.Warnings, err error) {
	ng.metrics.currentQueries.Inc()
	defer ng.metrics.currentQueries.Dec()

	ctx, cancel := context.WithTimeout(ctx, ng.timeout)
	q.cancel = cancel

	defer func() {
		ng.queryLoggerLock.RLock()
		if l := ng.queryLogger; l != nil {
			params := make(map[string]interface{}, 4)
			params["query"] = q.q
			if eq, ok := q.Statement().(*parser.EvalStmt); ok {
				params["start"] = formatDate(eq.Start)
				params["end"] = formatDate(eq.End)
				// The step provided by the user is in seconds.
				params["step"] = int64(eq.Interval / (time.Second / time.Nanosecond))
			}
			f := []interface{}{"params", params}
			if err != nil {
				f = append(f, "error", err)
			}
			f = append(f, "stats", stats.NewQueryStats(q.Stats()))
			if span := opentracing.SpanFromContext(ctx); span != nil {
				if spanCtx, ok := span.Context().(jaeger.SpanContext); ok {
					f = append(f, "spanID", spanCtx.SpanID())
				}
			}
			if origin := ctx.Value(queryOrigin{}); origin != nil {
				for k, v := range origin.(map[string]interface{}) {
					f = append(f, k, v)
				}
			}
			if err := l.Log(f...); err != nil {
				ng.metrics.queryLogFailures.Inc()
				level.Error(ng.logger).Log("msg", "can't log query", "err", err)
			}
		}
		ng.queryLoggerLock.RUnlock()
	}()

	execSpanTimer, ctx := q.stats.GetSpanTimer(ctx, stats.ExecTotalTime)
	defer execSpanTimer.Finish()

	queueSpanTimer, _ := q.stats.GetSpanTimer(ctx, stats.ExecQueueTime, ng.metrics.queryQueueTime)
	// Log query in active log. The active log guarantees that we don't run over
	// MaxConcurrent queries.
	if ng.activeQueryTracker != nil {
		queryIndex, err := ng.activeQueryTracker.Insert(ctx, q.q)
		if err != nil {
			queueSpanTimer.Finish()
			return nil, nil, contextErr(err, "query queue")
		}
		defer ng.activeQueryTracker.Delete(queryIndex)
	}
	queueSpanTimer.Finish()

	// Cancel when execution is done or an error was raised.
	defer q.cancel()

	const env = "query execution"

	evalSpanTimer, ctx := q.stats.GetSpanTimer(ctx, stats.EvalTotalTime)
	defer evalSpanTimer.Finish()

	// The base context might already be canceled on the first iteration (e.g. during shutdown).
	if err := contextDone(ctx, env); err != nil {
		return nil, nil, err
	}

	switch s := q.Statement().(type) {
	case *parser.EvalStmt:
		return ng.execEvalStmt(ctx, q, s)
	case parser.TestStmt:
		return nil, nil, s(ctx)
	}

	panic(errors.Errorf("promql.Engine.exec: unhandled statement of type %T", q.Statement()))
}

func timeMilliseconds(t time.Time) int64 {
	return t.UnixNano() / int64(time.Millisecond/time.Nanosecond)
}

func durationMilliseconds(d time.Duration) int64 {
	return int64(d / (time.Millisecond / time.Nanosecond))
}

// execEvalStmt evaluates the expression of an evaluation statement for the given time range.
func (ng *Engine) execEvalStmt(ctx context.Context, query *query, s *parser.EvalStmt) (parser.Value, storage.Warnings, error) {
	prepareSpanTimer, ctxPrepare := query.stats.GetSpanTimer(ctx, stats.QueryPreparationTime, ng.metrics.queryPrepareTime)
	mint, maxt := ng.findMinMaxTime(s)
	querier, err := query.queryable.Querier(ctxPrepare, mint, maxt)
	if err != nil {
		prepareSpanTimer.Finish()
		return nil, nil, err
	}
	defer querier.Close()

	topNode := ng.populateSeries(querier, s)
	prepareSpanTimer.Finish()

	// Modify the offset of vector and matrix selectors for the @ modifier
	// w.r.t. the start time since only 1 evaluation will be done on them.
	setOffsetForAtModifier(timeMilliseconds(s.Start), s.Expr)
	evalSpanTimer, ctxInnerEval := query.stats.GetSpanTimer(ctx, stats.InnerEvalTime, ng.metrics.queryInnerEval)
	// Instant evaluation. This is executed as a range evaluation with one step.
	if s.Start == s.End && s.Interval == 0 {
		start := timeMilliseconds(s.Start)
		evaluator := &evaluator{
			startTimestamp:           start,
			endTimestamp:             start,
			interval:                 1,
			ctx:                      ctxInnerEval,
			maxSamples:               ng.maxSamplesPerQuery,
			logger:                   ng.logger,
			lookbackDelta:            ng.lookbackDelta,
			topNode:                  topNode,
			noStepSubqueryIntervalFn: ng.noStepSubqueryIntervalFn,
		}

		val, warnings, err := evaluator.Eval(s.Expr)
		if err != nil {
			return nil, warnings, err
		}

		evalSpanTimer.Finish()

		var mat Matrix

		switch result := val.(type) {
		case Matrix:
			mat = result
		case String:
			return result, warnings, nil
		default:
			panic(errors.Errorf("promql.Engine.exec: invalid expression type %q", val.Type()))
		}

		query.matrix = mat
		switch s.Expr.Type() {
		case parser.ValueTypeVector:
			// Convert matrix with one value per series into vector.
			vector := make(Vector, len(mat))
			for i, s := range mat {
				// Point might have a different timestamp, force it to the evaluation
				// timestamp as that is when we ran the evaluation.
				vector[i] = Sample{Metric: s.Metric, Point: Point{V: s.Points[0].V, T: start}}
			}
			return vector, warnings, nil
		case parser.ValueTypeScalar:
			return Scalar{V: mat[0].Points[0].V, T: start}, warnings, nil
		case parser.ValueTypeMatrix:
			return mat, warnings, nil
		default:
			panic(errors.Errorf("promql.Engine.exec: unexpected expression type %q", s.Expr.Type()))
		}
	}

	// Range evaluation.
	evaluator := &evaluator{
		startTimestamp:           timeMilliseconds(s.Start),
		endTimestamp:             timeMilliseconds(s.End),
		interval:                 durationMilliseconds(s.Interval),
		ctx:                      ctxInnerEval,
		maxSamples:               ng.maxSamplesPerQuery,
		logger:                   ng.logger,
		lookbackDelta:            ng.lookbackDelta,
		noStepSubqueryIntervalFn: ng.noStepSubqueryIntervalFn,
		topNode:                  topNode,
	}
	val, warnings, err := evaluator.Eval(s.Expr)
	if err != nil {
		return nil, warnings, err
	}
	evalSpanTimer.Finish()

	mat, ok := val.(Matrix)
	if !ok {
		panic(errors.Errorf("promql.Engine.exec: invalid expression type %q", val.Type()))
	}
	query.matrix = mat

	if err := contextDone(ctx, "expression evaluation"); err != nil {
		return nil, warnings, err
	}

	// TODO(fabxc): where to ensure metric labels are a copy from the storage internals.
	sortSpanTimer, _ := query.stats.GetSpanTimer(ctx, stats.ResultSortTime, ng.metrics.queryResultSort)
	sort.Sort(mat)
	sortSpanTimer.Finish()

	return mat, warnings, nil
}

// subqueryTimes returns the sum of offsets and ranges of all subqueries in the path.
// If the @ modifier is used, then the offset and range is w.r.t. that timestamp
// (i.e. the sum is reset when we have @ modifier).
// The returned *int64 is the closest timestamp that was seen. nil for no @ modifier.
func subqueryTimes(path []parser.Node) (time.Duration, time.Duration, *int64) {
	var (
		subqOffset, subqRange time.Duration
		ts                    int64 = math.MaxInt64
	)
	for _, node := range path {
		switch n := node.(type) {
		case *parser.SubqueryExpr:
			subqOffset += n.OriginalOffset
			subqRange += n.Range
			if n.Timestamp != nil {
				// The @ modifier on subquery invalidates all the offset and
				// range till now. Hence resetting it here.
				subqOffset = n.OriginalOffset
				subqRange = n.Range
				ts = *n.Timestamp
			}
		}
	}
	var tsp *int64
	if ts != math.MaxInt64 {
		tsp = &ts
	}
	return subqOffset, subqRange, tsp
}

func (ng *Engine) findMinMaxTime(s *parser.EvalStmt) (int64, int64) {
	var minTimestamp, maxTimestamp int64 = math.MaxInt64, math.MinInt64
	// Whenever a MatrixSelector is evaluated, evalRange is set to the corresponding range.
	// The evaluation of the VectorSelector inside then evaluates the given range and unsets
	// the variable.
	var evalRange time.Duration
	parser.Inspect(s.Expr, func(node parser.Node, path []parser.Node) error {
		switch n := node.(type) {
		case *parser.VectorSelector:
			start, end := ng.getTimeRangesForSelector(s, n, path, evalRange)
			if start < minTimestamp {
				minTimestamp = start
			}
			if end > maxTimestamp {
				maxTimestamp = end
			}
			evalRange = 0

		case *parser.MatrixSelector:
			evalRange = n.Range
		}
		return nil
	})

	if maxTimestamp == math.MinInt64 {
		// This happens when there was no selector. Hence no time range to select.
		minTimestamp = 0
		maxTimestamp = 0
	}

	return minTimestamp, maxTimestamp
}

func (ng *Engine) getTimeRangesForSelector(s *parser.EvalStmt, n *parser.VectorSelector, path []parser.Node, evalRange time.Duration) (int64, int64) {
	start, end := timestamp.FromTime(s.Start), timestamp.FromTime(s.End)
	subqOffset, subqRange, subqTs := subqueryTimes(path)

	if subqTs != nil {
		// The timestamp on the subquery overrides the eval statement time ranges.
		start = *subqTs
		end = *subqTs
	}

	if n.Timestamp != nil {
		// The timestamp on the selector overrides everything.
		start = *n.Timestamp
		end = *n.Timestamp
	} else {
		offsetMilliseconds := durationMilliseconds(subqOffset)
		start = start - offsetMilliseconds - durationMilliseconds(subqRange)
		end = end - offsetMilliseconds
	}

	if evalRange == 0 {
		start = start - durationMilliseconds(ng.lookbackDelta)
	} else {
		// For all matrix queries we want to ensure that we have (end-start) + range selected
		// this way we have `range` data before the start time
		start = start - durationMilliseconds(evalRange)
	}

	offsetMilliseconds := durationMilliseconds(n.OriginalOffset)
	start = start - offsetMilliseconds
	end = end - offsetMilliseconds

	return start, end
}

func (ng *Engine) populateSeries(querier Querier, s *parser.EvalStmt) parser.Node {
	var (
		// Whenever a MatrixSelector is evaluated, evalRange is set to the corresponding range.
		// The evaluation of the VectorSelector inside then evaluates the given range and unsets
		// the variable.
		set       storage.SeriesSet
		evalRange time.Duration
		topNode   parser.Node
	)

	parser.Inspect(s.Expr, func(node parser.Node, path []parser.Node) error {
		switch n := node.(type) {
		case *parser.VectorSelector:
			start, end := ng.getTimeRangesForSelector(s, n, path, evalRange)
			hints := &storage.SelectHints{
				Start: start,
				End:   end,
				Step:  durationMilliseconds(s.Interval),
				Range: durationMilliseconds(evalRange),
				Func:  extractFuncFromPath(path),
			}

			evalRange = 0
			hints.By, hints.Grouping = extractGroupsFromPath(path)

			if topNode != nil {
				return fmt.Errorf("assigning topNode twice")
			}
			set, topNode = querier.Select(false, hints, path, n.LabelMatchers...)
			n.UnexpandedSeriesSet = set

		case *parser.MatrixSelector:
			evalRange = n.Range
		}
		return nil
	})
	return topNode
}

// extractFuncFromPath walks up the path and searches for the first instance of
// a function or aggregation.
func extractFuncFromPath(p []parser.Node) string {
	if len(p) == 0 {
		return ""
	}
	switch n := p[len(p)-1].(type) {
	case *parser.AggregateExpr:
		return n.Op.String()
	case *parser.Call:
		return n.Func.Name
	case *parser.BinaryExpr:
		// If we hit a binary expression we terminate since we only care about functions
		// or aggregations over a single metric.
		return ""
	}
	return extractFuncFromPath(p[:len(p)-1])
}

// extractGroupsFromPath parses vector outer function and extracts grouping information if by or without was used.
func extractGroupsFromPath(p []parser.Node) (bool, []string) {
	if len(p) == 0 {
		return false, nil
	}
	switch n := p[len(p)-1].(type) {
	case *parser.AggregateExpr:
		return !n.Without, n.Grouping
	}
	return false, nil
}

func checkAndExpandSeriesSet(ctx context.Context, expr parser.Expr) (storage.Warnings, error) {
	switch e := expr.(type) {
	case *parser.MatrixSelector:
		return checkAndExpandSeriesSet(ctx, e.VectorSelector)
	case *parser.VectorSelector:
		if e.Series != nil {
			return nil, nil
		}
		series, ws, err := expandSeriesSet(ctx, e.UnexpandedSeriesSet)
		e.Series = series
		return ws, err
	}
	return nil, nil
}

func expandSeriesSet(ctx context.Context, it storage.SeriesSet) (res []storage.Series, ws storage.Warnings, err error) {
	for it.Next() {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}
		res = append(res, it.At())
	}
	return res, it.Warnings(), it.Err()
}

type errWithWarnings struct {
	err      error
	warnings storage.Warnings
}

func (e errWithWarnings) Error() string { return e.err.Error() }

// An evaluator evaluates given expressions over given fixed timestamps. It
// is attached to an engine through which it connects to a querier and reports
// errors. On timeout or cancellation of its context it terminates.
type evaluator struct {
	ctx context.Context

	startTimestamp int64 // Start time in milliseconds.
	endTimestamp   int64 // End time in milliseconds.
	interval       int64 // Interval in milliseconds.

	maxSamples               int64
	currentSamples           int64
	logger                   log.Logger
	lookbackDelta            time.Duration
	topNode                  parser.Node
	noStepSubqueryIntervalFn func(rangeMillis int64) int64
}

// errorf causes a panic with the input formatted into an error.
func (ev *evaluator) errorf(format string, args ...interface{}) {
	ev.error(errors.Errorf(format, args...))
}

// error causes a panic with the given error.
func (ev *evaluator) error(err error) {
	panic(err)
}

// recover is the handler that turns panics into returns from the top level of evaluation.
func (ev *evaluator) recover(ws *storage.Warnings, errp *error) {
	e := recover()
	if e == nil {
		return
	}

	switch err := e.(type) {
	case runtime.Error:
		// Print the stack trace but do not inhibit the running application.
		buf := make([]byte, 64<<10)
		buf = buf[:runtime.Stack(buf, false)]

		level.Error(ev.logger).Log("msg", "runtime panic in parser", "err", e, "stacktrace", string(buf))
		*errp = errors.Wrap(err, "unexpected error")
	case errWithWarnings:
		*errp = err.err
		*ws = append(*ws, err.warnings...)
	default:
		*errp = e.(error)
	}
}

func (ev *evaluator) Eval(expr parser.Expr) (v parser.Value, ws storage.Warnings, err error) {
	defer ev.recover(&ws, &err)

	v, ws = ev.eval(expr)
	return v, ws, nil
}

// EvalSeriesHelper stores extra information about a series.
type EvalSeriesHelper struct {
	// The grouping key used by aggregation.
	groupingKey uint64
}

// EvalNodeHelper stores extra information and caches for evaluating a single node across steps.
type EvalNodeHelper struct {
	// Evaluation timestamp.
	Ts int64
	// Vector that can be used for output.
	Out Vector

	// Caches.
	// DropMetricName and label_*.
	Dmn map[uint64]labels.Labels
	// signatureFunc.
	sigf map[string]string
	// funcHistogramQuantile.
	signatureToMetricWithBuckets map[string]*metricWithBuckets
	// label_replace.
	regex *regexp.Regexp

	lb           *labels.Builder
	lblBuf       []byte
	lblResultBuf []byte

	// For binary vector matching.
	rightSigs    map[string]Sample
	matchedSigs  map[string]map[uint64]struct{}
	resultMetric map[string]labels.Labels
}

// DropMetricName is a cached version of DropMetricName.
func (enh *EvalNodeHelper) DropMetricName(l labels.Labels) labels.Labels {
	if enh.Dmn == nil {
		enh.Dmn = make(map[uint64]labels.Labels, len(enh.Out))
	}
	h := l.Hash()
	ret, ok := enh.Dmn[h]
	if ok {
		return ret
	}
	ret = dropMetricName(l)
	enh.Dmn[h] = ret
	return ret
}

// signatureFunc is a cached version of signatureFunc.
func (enh *EvalNodeHelper) signatureFunc(on bool, names ...string) func(labels.Labels) string {
	if enh.sigf == nil {
		enh.sigf = make(map[string]string, len(enh.Out))
	}
	f := signatureFunc(on, enh.lblBuf, names...)
	return func(l labels.Labels) string {
		enh.lblBuf = l.Bytes(enh.lblBuf)
		ret, ok := enh.sigf[string(enh.lblBuf)]
		if ok {
			return ret
		}
		ret = f(l)
		enh.sigf[string(enh.lblBuf)] = ret
		return ret
	}
}

// rangeEval evaluates the given expressions, and then for each step calls
// the given funcCall with the values computed for each expression at that
// step. The return value is the combination into time series of all the
// function call results.
func (ev *evaluator) rangeEval(prepSeries func(labels.Labels, *EvalSeriesHelper), funcCall func([]parser.Value, [][]EvalSeriesHelper, *EvalNodeHelper) (Vector, storage.Warnings), exprs ...parser.Expr) (Matrix, storage.Warnings) {
	numSteps := int((ev.endTimestamp-ev.startTimestamp)/ev.interval) + 1
	matrixes := make([]Matrix, len(exprs))
	origMatrixes := make([]Matrix, len(exprs))
	originalNumSamples := ev.currentSamples

	var warnings storage.Warnings
	for i, e := range exprs {
		// Functions will take string arguments from the expressions, not the values.
		if e != nil && e.Type() != parser.ValueTypeString {
			// ev.currentSamples will be updated to the correct value within the ev.eval call.
			val, ws := ev.eval(e)
			warnings = append(warnings, ws...)
			matrixes[i] = val.(Matrix)

			// Keep a copy of the original point slices so that they
			// can be returned to the pool.
			origMatrixes[i] = make(Matrix, len(matrixes[i]))
			copy(origMatrixes[i], matrixes[i])
		}
	}

	vectors := make([]Vector, len(exprs))    // Input vectors for the function.
	args := make([]parser.Value, len(exprs)) // Argument to function.
	// Create an output vector that is as big as the input matrix with
	// the most time series.
	biggestLen := 1
	for i := range exprs {
		vectors[i] = make(Vector, 0, len(matrixes[i]))
		if len(matrixes[i]) > biggestLen {
			biggestLen = len(matrixes[i])
		}
	}
	enh := &EvalNodeHelper{Out: make(Vector, 0, biggestLen)}
	seriess := make(map[uint64]Series, biggestLen) // Output series by series hash.
	tempNumSamples := ev.currentSamples

	var (
		seriesHelpers [][]EvalSeriesHelper
		bufHelpers    [][]EvalSeriesHelper // Buffer updated on each step
	)

	// If the series preparation function is provided, we should run it for
	// every single series in the matrix.
	if prepSeries != nil {
		seriesHelpers = make([][]EvalSeriesHelper, len(exprs))
		bufHelpers = make([][]EvalSeriesHelper, len(exprs))

		for i := range exprs {
			seriesHelpers[i] = make([]EvalSeriesHelper, len(matrixes[i]))
			bufHelpers[i] = make([]EvalSeriesHelper, len(matrixes[i]))

			for si, series := range matrixes[i] {
				h := seriesHelpers[i][si]
				prepSeries(series.Metric, &h)
				seriesHelpers[i][si] = h
			}
		}
	}

	for ts := ev.startTimestamp; ts <= ev.endTimestamp; ts += ev.interval {
		if err := contextDone(ev.ctx, "expression evaluation"); err != nil {
			ev.error(err)
		}
		// Reset number of samples in memory after each timestamp.
		ev.currentSamples = tempNumSamples
		// Gather input vectors for this timestamp.
		for i := range exprs {
			vectors[i] = vectors[i][:0]

			if prepSeries != nil {
				bufHelpers[i] = bufHelpers[i][:0]
			}

			for si, series := range matrixes[i] {
				for _, point := range series.Points {
					if point.T == ts {
						if ev.currentSamples < ev.maxSamples {
							vectors[i] = append(vectors[i], Sample{Metric: series.Metric, Point: point})
							if prepSeries != nil {
								bufHelpers[i] = append(bufHelpers[i], seriesHelpers[i][si])
							}

							// Move input vectors forward so we don't have to re-scan the same
							// past points at the next step.
							matrixes[i][si].Points = series.Points[1:]
							ev.currentSamples++
						} else {
							ev.error(ErrTooManySamples(env))
						}
					}
					break
				}
			}
			args[i] = vectors[i]
		}

		// Make the function call.
		enh.Ts = ts
		result, ws := funcCall(args, bufHelpers, enh)
		if result.ContainsSameLabelset() {
			ev.errorf("vector cannot contain metrics with the same labelset")
		}
		enh.Out = result[:0] // Reuse result vector.
		warnings = append(warnings, ws...)

		ev.currentSamples += int64(len(result))
		// When we reset currentSamples to tempNumSamples during the next iteration of the loop it also
		// needs to include the samples from the result here, as they're still in memory.
		tempNumSamples += int64(len(result))

		if ev.currentSamples > ev.maxSamples {
			ev.error(ErrTooManySamples(env))
		}

		// If this could be an instant query, shortcut so as not to change sort order.
		if ev.endTimestamp == ev.startTimestamp {
			mat := make(Matrix, len(result))
			for i, s := range result {
				s.Point.T = ts
				mat[i] = Series{Metric: s.Metric, Points: []Point{s.Point}}
			}
			ev.currentSamples = int64(originalNumSamples + mat.TotalSamples())
			return mat, warnings
		}

		// Add samples in output vector to output series.
		for _, sample := range result {
			h := sample.Metric.Hash()
			ss, ok := seriess[h]
			if !ok {
				ss = Series{
					Metric: sample.Metric,
					Points: getPointSlice(numSteps),
				}
			}
			sample.Point.T = ts
			ss.Points = append(ss.Points, sample.Point)
			seriess[h] = ss

		}
	}

	// Reuse the original point slices.
	for _, m := range origMatrixes {
		for _, s := range m {
			putPointSlice(s.Points)
		}
	}
	// Assemble the output matrix. By the time we get here we know we don't have too many samples.
	mat := make(Matrix, 0, len(seriess))
	for _, ss := range seriess {
		mat = append(mat, ss)
	}
	ev.currentSamples = originalNumSamples + mat.TotalSamples()
	return mat, warnings
}

// evalSubquery evaluates given SubqueryExpr and returns an equivalent
// evaluated MatrixSelector in its place. Note that the Name and LabelMatchers are not set.
func (ev *evaluator) evalSubquery(subq *parser.SubqueryExpr) (*parser.MatrixSelector, int, storage.Warnings) {
	val, ws := ev.eval(subq)
	mat := val.(Matrix)
	vs := &parser.VectorSelector{
		OriginalOffset: subq.OriginalOffset,
		Offset:         subq.Offset,
		Series:         make([]storage.Series, 0, len(mat)),
		Timestamp:      subq.Timestamp,
	}
	if subq.Timestamp != nil {
		// The offset of subquery is not modified in case of @ modifier.
		// Hence we take care of that here for the result.
		vs.Offset = subq.OriginalOffset + time.Duration(ev.startTimestamp-*subq.Timestamp)*time.Millisecond
	}
	ms := &parser.MatrixSelector{
		Range:          subq.Range,
		VectorSelector: vs,
	}
	totalSamples := 0
	for _, s := range mat {
		totalSamples += len(s.Points)
		vs.Series = append(vs.Series, NewStorageSeries(s))
	}
	return ms, totalSamples, ws
}

func (ev *evaluator) getPushdownResult(vs *parser.VectorSelector, numSteps int) Matrix {
	mat := make(Matrix, 0, len(vs.Series))
	for i, s := range vs.Series {
		ss := Series{
			Metric: dropMetricName(vs.Series[i].Labels()),
			Points: getPointSlice(numSteps),
		}

		it := s.Iterator()
		for it.Next() {
			t, v := it.At()
			ss.Points = append(ss.Points, Point{V: v, T: t})
		}
		if len(ss.Points) > 0 {
			mat = append(mat, ss)
		} else {
			putPointSlice(ss.Points)
		}
	}
	return mat
}

// eval evaluates the given expression as the given AST expression node requires.
func (ev *evaluator) eval(expr parser.Expr) (parser.Value, storage.Warnings) {
	// This is the top-level evaluation method.
	// Thus, we check for timeout/cancellation here.
	if err := contextDone(ev.ctx, "expression evaluation"); err != nil {
		ev.error(err)
	}
	numSteps := int((ev.endTimestamp-ev.startTimestamp)/ev.interval) + 1

	// Create a new span to help investigate inner evaluation performances.
	span, _ := opentracing.StartSpanFromContext(ev.ctx, stats.InnerEvalTime.SpanOperation()+" eval "+reflect.TypeOf(expr).String())
	defer span.Finish()

	if expr == ev.topNode {
		/* the storage layer has already processed this node. Just return
		the result. */
		var (
			mat      Matrix
			warnings storage.Warnings
			err      error
		)
		parser.Inspect(expr, func(node parser.Node, path []parser.Node) error {
			switch n := node.(type) {
			/* Note that a MatrixSelector cascades down to it's VectorSelector in Inspect */
			case *parser.VectorSelector:
				warnings, err = checkAndExpandSeriesSet(ev.ctx, n)
				if err != nil {
					err = fmt.Errorf("topNode eval vector-selector: %w", err)
					return err
				}
				if mat != nil {
					err = fmt.Errorf("Matrix is already filled in")
					return err
				}
				mat = ev.getPushdownResult(n, numSteps)
			}
			return nil
		})
		if err != nil {
			ev.error(err)
		}
		if mat == nil {
			ev.error(fmt.Errorf("Matrix not filled in"))
		}
		return mat, warnings
	}

	switch e := expr.(type) {
	case *parser.AggregateExpr:
		// Grouping labels must be sorted (expected both by generateGroupingKey() and aggregation()).
		sortedGrouping := e.Grouping
		sort.Strings(sortedGrouping)

		// Prepare a function to initialise series helpers with the grouping key.
		buf := make([]byte, 0, 1024)
		initSeries := func(series labels.Labels, h *EvalSeriesHelper) {
			h.groupingKey, buf = generateGroupingKey(series, sortedGrouping, e.Without, buf)
		}

		unwrapParenExpr(&e.Param)
		if s, ok := unwrapStepInvariantExpr(e.Param).(*parser.StringLiteral); ok {
			return ev.rangeEval(initSeries, func(v []parser.Value, sh [][]EvalSeriesHelper, enh *EvalNodeHelper) (Vector, storage.Warnings) {
				return ev.aggregation(e.Op, sortedGrouping, e.Without, s.Val, v[0].(Vector), sh[0], enh), nil
			}, e.Expr)
		}
		return ev.rangeEval(initSeries, func(v []parser.Value, sh [][]EvalSeriesHelper, enh *EvalNodeHelper) (Vector, storage.Warnings) {
			var param float64
			if e.Param != nil {
				param = v[0].(Vector)[0].V
			}
			return ev.aggregation(e.Op, sortedGrouping, e.Without, param, v[1].(Vector), sh[1], enh), nil
		}, e.Param, e.Expr)

	case *parser.Call:
		call := FunctionCalls[e.Func.Name]
		if e.Func.Name == "timestamp" {
			// Matrix evaluation always returns the evaluation time,
			// so this function needs special handling when given
			// a vector selector.
			unwrapParenExpr(&e.Args[0])
			arg := unwrapStepInvariantExpr(e.Args[0])
			vs, ok := arg.(*parser.VectorSelector)
			if ok {
				return ev.rangeEval(nil, func(v []parser.Value, _ [][]EvalSeriesHelper, enh *EvalNodeHelper) (Vector, storage.Warnings) {
					if vs.Timestamp != nil {
						// This is a special case only for "timestamp" since the offset
						// needs to be adjusted for every point.
						vs.Offset = time.Duration(enh.Ts-*vs.Timestamp) * time.Millisecond
					}
					val, ws := ev.vectorSelector(vs, enh.Ts)
					return call([]parser.Value{val}, e.Args, enh), ws
				})
			}
		}

		// Check if the function has a matrix argument.
		var (
			matrixArgIndex int
			matrixArg      bool
			warnings       storage.Warnings
		)
		for i := range e.Args {
			unwrapParenExpr(&e.Args[i])
			a := unwrapStepInvariantExpr(e.Args[i])
			if _, ok := a.(*parser.MatrixSelector); ok {
				matrixArgIndex = i
				matrixArg = true
				break
			}
			// parser.SubqueryExpr can be used in place of parser.MatrixSelector.
			if subq, ok := a.(*parser.SubqueryExpr); ok {
				matrixArgIndex = i
				matrixArg = true
				// Replacing parser.SubqueryExpr with parser.MatrixSelector.
				val, totalSamples, ws := ev.evalSubquery(subq)
				e.Args[i] = val
				warnings = append(warnings, ws...)
				defer func() {
					// subquery result takes space in the memory. Get rid of that at the end.
					val.VectorSelector.(*parser.VectorSelector).Series = nil
					ev.currentSamples -= int64(totalSamples)
				}()
				break
			}
		}
		if !matrixArg {
			// Does not have a matrix argument.
			return ev.rangeEval(nil, func(v []parser.Value, _ [][]EvalSeriesHelper, enh *EvalNodeHelper) (Vector, storage.Warnings) {
				return call(v, e.Args, enh), warnings
			}, e.Args...)
		}

		inArgs := make([]parser.Value, len(e.Args))
		// Evaluate any non-matrix arguments.
		otherArgs := make([]Matrix, len(e.Args))
		otherInArgs := make([]Vector, len(e.Args))
		for i, e := range e.Args {
			if i != matrixArgIndex {
				val, ws := ev.eval(e)
				otherArgs[i] = val.(Matrix)
				otherInArgs[i] = Vector{Sample{}}
				inArgs[i] = otherInArgs[i]
				warnings = append(warnings, ws...)
			}
		}

		sel := unwrapStepInvariantExpr(e.Args[matrixArgIndex]).(*parser.MatrixSelector)
		selVS := sel.VectorSelector.(*parser.VectorSelector)

		ws, err := checkAndExpandSeriesSet(ev.ctx, sel)
		warnings = append(warnings, ws...)
		if err != nil {
			ev.error(errWithWarnings{errors.Wrap(err, "expanding series"), warnings})
		}
		mat := make(Matrix, 0, len(selVS.Series)) // Output matrix.
		offset := durationMilliseconds(selVS.Offset)
		selRange := durationMilliseconds(sel.Range)
		stepRange := selRange
		if stepRange > ev.interval {
			stepRange = ev.interval
		}
		// Reuse objects across steps to save memory allocations.
		points := getPointSlice(16)
		inMatrix := make(Matrix, 1)
		inArgs[matrixArgIndex] = inMatrix
		enh := &EvalNodeHelper{Out: make(Vector, 0, 1)}
		// Process all the calls for one time series at a time.
		it := storage.NewBuffer(selRange)
		for i, s := range selVS.Series {
			ev.currentSamples -= int64(len(points))
			points = points[:0]
			it.Reset(s.Iterator())
			metric := selVS.Series[i].Labels()
			// The last_over_time function acts like offset; thus, it
			// should keep the metric name.  For all the other range
			// vector functions, the only change needed is to drop the
			// metric name in the output.
			if e.Func.Name != "last_over_time" {
				metric = dropMetricName(metric)
			}
			ss := Series{
				// For all range vector functions, the only change to the
				// output labels is dropping the metric name so just do
				// it once here.
				Metric: metric,
				Points: getPointSlice(numSteps),
			}
			inMatrix[0].Metric = selVS.Series[i].Labels()
			for ts, step := ev.startTimestamp, -1; ts <= ev.endTimestamp; ts += ev.interval {
				step++
				// Set the non-matrix arguments.
				// They are scalar, so it is safe to use the step number
				// when looking up the argument, as there will be no gaps.
				for j := range e.Args {
					if j != matrixArgIndex {
						otherInArgs[j][0].V = otherArgs[j][0].Points[step].V
					}
				}
				maxt := ts - offset
				mint := maxt - selRange
				// Evaluate the matrix selector for this series for this step.
				points = ev.matrixIterSlice(it, mint, maxt, points)
				if len(points) == 0 {
					continue
				}
				inMatrix[0].Points = points
				enh.Ts = ts
				// Make the function call.
				outVec := call(inArgs, e.Args, enh)
				enh.Out = outVec[:0]
				if len(outVec) > 0 {
					ss.Points = append(ss.Points, Point{V: outVec[0].Point.V, T: ts})
				}
				// Only buffer stepRange milliseconds from the second step on.
				it.ReduceDelta(stepRange)
			}
			if len(ss.Points) > 0 {
				if ev.currentSamples+int64(len(ss.Points)) <= ev.maxSamples {
					mat = append(mat, ss)
					ev.currentSamples += int64(len(ss.Points))
				} else {
					ev.error(ErrTooManySamples(env))
				}
			} else {
				putPointSlice(ss.Points)
			}
		}

		ev.currentSamples -= int64(len(points))
		putPointSlice(points)

		// The absent_over_time function returns 0 or 1 series. So far, the matrix
		// contains multiple series. The following code will create a new series
		// with values of 1 for the timestamps where no series has value.
		if e.Func.Name == "absent_over_time" {
			steps := int(1 + (ev.endTimestamp-ev.startTimestamp)/ev.interval)
			// Iterate once to look for a complete series.
			for _, s := range mat {
				if len(s.Points) == steps {
					return Matrix{}, warnings
				}
			}

			found := map[int64]struct{}{}

			for i, s := range mat {
				for _, p := range s.Points {
					found[p.T] = struct{}{}
				}
				if i > 0 && len(found) == steps {
					return Matrix{}, warnings
				}
			}

			newp := make([]Point, 0, steps-len(found))
			for ts := ev.startTimestamp; ts <= ev.endTimestamp; ts += ev.interval {
				if _, ok := found[ts]; !ok {
					newp = append(newp, Point{T: ts, V: 1})
				}
			}

			return Matrix{
				Series{
					Metric: createLabelsForAbsentFunction(e.Args[0]),
					Points: newp,
				},
			}, warnings
		}

		if mat.ContainsSameLabelset() {
			ev.errorf("vector cannot contain metrics with the same labelset")
		}

		return mat, warnings

	case *parser.ParenExpr:
		return ev.eval(e.Expr)

	case *parser.UnaryExpr:
		val, ws := ev.eval(e.Expr)
		mat := val.(Matrix)
		if e.Op == parser.SUB {
			for i := range mat {
				mat[i].Metric = dropMetricName(mat[i].Metric)
				for j := range mat[i].Points {
					mat[i].Points[j].V = -mat[i].Points[j].V
				}
			}
			if mat.ContainsSameLabelset() {
				ev.errorf("vector cannot contain metrics with the same labelset")
			}
		}
		return mat, ws

	case *parser.BinaryExpr:
		switch lt, rt := e.LHS.Type(), e.RHS.Type(); {
		case lt == parser.ValueTypeScalar && rt == parser.ValueTypeScalar:
			return ev.rangeEval(nil, func(v []parser.Value, _ [][]EvalSeriesHelper, enh *EvalNodeHelper) (Vector, storage.Warnings) {
				val := scalarBinop(e.Op, v[0].(Vector)[0].Point.V, v[1].(Vector)[0].Point.V)
				return append(enh.Out, Sample{Point: Point{V: val}}), nil
			}, e.LHS, e.RHS)
		case lt == parser.ValueTypeVector && rt == parser.ValueTypeVector:
			switch e.Op {
			case parser.LAND:
				return ev.rangeEval(nil, func(v []parser.Value, _ [][]EvalSeriesHelper, enh *EvalNodeHelper) (Vector, storage.Warnings) {
					return ev.VectorAnd(v[0].(Vector), v[1].(Vector), e.VectorMatching, enh), nil
				}, e.LHS, e.RHS)
			case parser.LOR:
				return ev.rangeEval(nil, func(v []parser.Value, _ [][]EvalSeriesHelper, enh *EvalNodeHelper) (Vector, storage.Warnings) {
					return ev.VectorOr(v[0].(Vector), v[1].(Vector), e.VectorMatching, enh), nil
				}, e.LHS, e.RHS)
			case parser.LUNLESS:
				return ev.rangeEval(nil, func(v []parser.Value, _ [][]EvalSeriesHelper, enh *EvalNodeHelper) (Vector, storage.Warnings) {
					return ev.VectorUnless(v[0].(Vector), v[1].(Vector), e.VectorMatching, enh), nil
				}, e.LHS, e.RHS)
			default:
				return ev.rangeEval(nil, func(v []parser.Value, _ [][]EvalSeriesHelper, enh *EvalNodeHelper) (Vector, storage.Warnings) {
					return ev.VectorBinop(e.Op, v[0].(Vector), v[1].(Vector), e.VectorMatching, e.ReturnBool, enh), nil
				}, e.LHS, e.RHS)
			}

		case lt == parser.ValueTypeVector && rt == parser.ValueTypeScalar:
			return ev.rangeEval(nil, func(v []parser.Value, _ [][]EvalSeriesHelper, enh *EvalNodeHelper) (Vector, storage.Warnings) {
				return ev.VectorscalarBinop(e.Op, v[0].(Vector), Scalar{V: v[1].(Vector)[0].Point.V}, false, e.ReturnBool, enh), nil
			}, e.LHS, e.RHS)

		case lt == parser.ValueTypeScalar && rt == parser.ValueTypeVector:
			return ev.rangeEval(nil, func(v []parser.Value, _ [][]EvalSeriesHelper, enh *EvalNodeHelper) (Vector, storage.Warnings) {
				return ev.VectorscalarBinop(e.Op, v[1].(Vector), Scalar{V: v[0].(Vector)[0].Point.V}, true, e.ReturnBool, enh), nil
			}, e.LHS, e.RHS)
		}

	case *parser.NumberLiteral:
		return ev.rangeEval(nil, func(v []parser.Value, _ [][]EvalSeriesHelper, enh *EvalNodeHelper) (Vector, storage.Warnings) {
			return append(enh.Out, Sample{Point: Point{V: e.Val}}), nil
		})

	case *parser.StringLiteral:
		return String{V: e.Val, T: ev.startTimestamp}, nil

	case *parser.VectorSelector:
		ws, err := checkAndExpandSeriesSet(ev.ctx, e)
		if err != nil {
			ev.error(errWithWarnings{errors.Wrap(err, "expanding series"), ws})
		}
		mat := make(Matrix, 0, len(e.Series))
		it := storage.NewMemoizedEmptyIterator(durationMilliseconds(ev.lookbackDelta))
		for i, s := range e.Series {
			it.Reset(s.Iterator())
			ss := Series{
				Metric: e.Series[i].Labels(),
				Points: getPointSlice(numSteps),
			}

			for ts := ev.startTimestamp; ts <= ev.endTimestamp; ts += ev.interval {
				_, v, ok := ev.vectorSelectorSingle(it, e, ts)
				if ok {
					if ev.currentSamples < ev.maxSamples {
						ss.Points = append(ss.Points, Point{V: v, T: ts})
						ev.currentSamples++
					} else {
						ev.error(ErrTooManySamples(env))
					}
				}
			}

			if len(ss.Points) > 0 {
				mat = append(mat, ss)
			} else {
				putPointSlice(ss.Points)
			}
		}
		return mat, ws

	case *parser.MatrixSelector:
		if ev.startTimestamp != ev.endTimestamp {
			panic(errors.New("cannot do range evaluation of matrix selector"))
		}
		return ev.matrixSelector(e)

	case *parser.SubqueryExpr:
		offsetMillis := durationMilliseconds(e.Offset)
		rangeMillis := durationMilliseconds(e.Range)
		newEv := &evaluator{
			endTimestamp:             ev.endTimestamp - offsetMillis,
			ctx:                      ev.ctx,
			currentSamples:           ev.currentSamples,
			maxSamples:               ev.maxSamples,
			logger:                   ev.logger,
			lookbackDelta:            ev.lookbackDelta,
			noStepSubqueryIntervalFn: ev.noStepSubqueryIntervalFn,
		}

		if e.Step != 0 {
			newEv.interval = durationMilliseconds(e.Step)
		} else {
			newEv.interval = ev.noStepSubqueryIntervalFn(rangeMillis)
		}

		// Start with the first timestamp after (ev.startTimestamp - offset - range)
		// that is aligned with the step (multiple of 'newEv.interval').
		newEv.startTimestamp = newEv.interval * ((ev.startTimestamp - offsetMillis - rangeMillis) / newEv.interval)
		if newEv.startTimestamp < (ev.startTimestamp - offsetMillis - rangeMillis) {
			newEv.startTimestamp += newEv.interval
		}

		if newEv.startTimestamp != ev.startTimestamp {
			// Adjust the offset of selectors based on the new
			// start time of the evaluator since the calculation
			// of the offset with @ happens w.r.t. the start time.
			setOffsetForAtModifier(newEv.startTimestamp, e.Expr)
		}

		res, ws := newEv.eval(e.Expr)
		ev.currentSamples = newEv.currentSamples
		return res, ws

	case *parser.StepInvariantExpr:
		switch ce := e.Expr.(type) {
		case *parser.StringLiteral, *parser.NumberLiteral:
			return ev.eval(ce)
		}

		newEv := &evaluator{
			startTimestamp:           ev.startTimestamp,
			endTimestamp:             ev.startTimestamp, // Always a single evaluation.
			interval:                 ev.interval,
			ctx:                      ev.ctx,
			currentSamples:           ev.currentSamples,
			maxSamples:               ev.maxSamples,
			logger:                   ev.logger,
			lookbackDelta:            ev.lookbackDelta,
			noStepSubqueryIntervalFn: ev.noStepSubqueryIntervalFn,
		}
		res, ws := newEv.eval(e.Expr)
		ev.currentSamples = newEv.currentSamples
		switch e.Expr.(type) {
		case *parser.MatrixSelector, *parser.SubqueryExpr:
			// We do not duplicate results for range selectors since result is a matrix
			// with their unique timestamps which does not depend on the step.
			return res, ws
		}

		// For every evaluation while the value remains same, the timestamp for that
		// value would change for different eval times. Hence we duplicate the result
		// with changed timestamps.
		mat, ok := res.(Matrix)
		if !ok {
			panic(errors.Errorf("unexpected result in StepInvariantExpr evaluation: %T", expr))
		}
		for i := range mat {
			if len(mat[i].Points) != 1 {
				panic(errors.Errorf("unexpected number of samples"))
			}
			for ts := ev.startTimestamp + ev.interval; ts <= ev.endTimestamp; ts = ts + ev.interval {
				mat[i].Points = append(mat[i].Points, Point{
					T: ts,
					V: mat[i].Points[0].V,
				})
				ev.currentSamples++
				if ev.currentSamples > ev.maxSamples {
					ev.error(ErrTooManySamples(env))
				}
			}
		}
		return res, ws
	}

	panic(errors.Errorf("unhandled expression of type: %T", expr))
}

// vectorSelector evaluates a *parser.VectorSelector expression.
func (ev *evaluator) vectorSelector(node *parser.VectorSelector, ts int64) (Vector, storage.Warnings) {
	ws, err := checkAndExpandSeriesSet(ev.ctx, node)
	if err != nil {
		ev.error(errWithWarnings{errors.Wrap(err, "expanding series"), ws})
	}
	vec := make(Vector, 0, len(node.Series))
	it := storage.NewMemoizedEmptyIterator(durationMilliseconds(ev.lookbackDelta))
	for i, s := range node.Series {
		it.Reset(s.Iterator())

		t, v, ok := ev.vectorSelectorSingle(it, node, ts)
		if ok {
			vec = append(vec, Sample{
				Metric: node.Series[i].Labels(),
				Point:  Point{V: v, T: t},
			})

			ev.currentSamples++
			if ev.currentSamples > ev.maxSamples {
				ev.error(ErrTooManySamples(env))
			}
		}

	}
	return vec, ws
}

// vectorSelectorSingle evaluates a instant vector for the iterator of one time series.
func (ev *evaluator) vectorSelectorSingle(it *storage.MemoizedSeriesIterator, node *parser.VectorSelector, ts int64) (int64, float64, bool) {
	refTime := ts - durationMilliseconds(node.Offset)
	var t int64
	var v float64

	ok := it.Seek(refTime)
	if !ok {
		if it.Err() != nil {
			ev.error(it.Err())
		}
	}

	if ok {
		t, v = it.Values()
	}

	if !ok || t > refTime {
		t, v, ok = it.PeekPrev()
		if !ok || t < refTime-durationMilliseconds(ev.lookbackDelta) {
			return 0, 0, false
		}
	}
	if value.IsStaleNaN(v) {
		return 0, 0, false
	}
	return t, v, true
}

var pointPool = sync.Pool{}

func getPointSlice(sz int) []Point {
	p := pointPool.Get()
	if p != nil {
		return p.([]Point)
	}
	return make([]Point, 0, sz)
}

func putPointSlice(p []Point) {
	//nolint:staticcheck // Ignore SA6002 relax staticcheck verification.
	pointPool.Put(p[:0])
}

// matrixSelector evaluates a *parser.MatrixSelector expression.
func (ev *evaluator) matrixSelector(node *parser.MatrixSelector) (Matrix, storage.Warnings) {
	var (
		vs = node.VectorSelector.(*parser.VectorSelector)

		offset = durationMilliseconds(vs.Offset)
		maxt   = ev.startTimestamp - offset
		mint   = maxt - durationMilliseconds(node.Range)
		matrix = make(Matrix, 0, len(vs.Series))

		it = storage.NewBuffer(durationMilliseconds(node.Range))
	)
	ws, err := checkAndExpandSeriesSet(ev.ctx, node)
	if err != nil {
		ev.error(errWithWarnings{errors.Wrap(err, "expanding series"), ws})
	}

	series := vs.Series
	for i, s := range series {
		if err := contextDone(ev.ctx, "expression evaluation"); err != nil {
			ev.error(err)
		}
		it.Reset(s.Iterator())
		ss := Series{
			Metric: series[i].Labels(),
		}

		ss.Points = ev.matrixIterSlice(it, mint, maxt, getPointSlice(16))

		if len(ss.Points) > 0 {
			matrix = append(matrix, ss)
		} else {
			putPointSlice(ss.Points)
		}
	}
	return matrix, ws
}

// matrixIterSlice populates a matrix vector covering the requested range for a
// single time series, with points retrieved from an iterator.
//
// As an optimization, the matrix vector may already contain points of the same
// time series from the evaluation of an earlier step (with lower mint and maxt
// values). Any such points falling before mint are discarded; points that fall
// into the [mint, maxt] range are retained; only points with later timestamps
// are populated from the iterator.
func (ev *evaluator) matrixIterSlice(it *storage.BufferedSeriesIterator, mint, maxt int64, out []Point) []Point {
	if len(out) > 0 && out[len(out)-1].T >= mint {
		// There is an overlap between previous and current ranges, retain common
		// points. In most such cases:
		//   (a) the overlap is significantly larger than the eval step; and/or
		//   (b) the number of samples is relatively small.
		// so a linear search will be as fast as a binary search.
		var drop int
		for drop = 0; out[drop].T < mint; drop++ {
		}
		ev.currentSamples -= int64(drop)
		copy(out, out[drop:])
		out = out[:len(out)-drop]
		// Only append points with timestamps after the last timestamp we have.
		mint = out[len(out)-1].T + 1
	} else {
		ev.currentSamples -= int64(len(out))
		out = out[:0]
	}

	ok := it.Seek(maxt)
	if !ok {
		if it.Err() != nil {
			ev.error(it.Err())
		}
	}

	buf := it.Buffer()
	for buf.Next() {
		t, v := buf.At()
		if value.IsStaleNaN(v) {
			continue
		}
		// Values in the buffer are guaranteed to be smaller than maxt.
		if t >= mint {
			if ev.currentSamples >= ev.maxSamples {
				ev.error(ErrTooManySamples(env))
			}
			ev.currentSamples++
			out = append(out, Point{T: t, V: v})
		}
	}
	// The seeked sample might also be in the range.
	if ok {
		t, v := it.Values()
		if t == maxt && !value.IsStaleNaN(v) {
			if ev.currentSamples >= ev.maxSamples {
				ev.error(ErrTooManySamples(env))
			}
			out = append(out, Point{T: t, V: v})
			ev.currentSamples++
		}
	}
	return out
}

func (ev *evaluator) VectorAnd(lhs, rhs Vector, matching *parser.VectorMatching, enh *EvalNodeHelper) Vector {
	if matching.Card != parser.CardManyToMany {
		panic("set operations must only use many-to-many matching")
	}
	sigf := enh.signatureFunc(matching.On, matching.MatchingLabels...)

	// The set of signatures for the right-hand side Vector.
	rightSigs := map[string]struct{}{}
	// Add all rhs samples to a map so we can easily find matches later.
	for _, rs := range rhs {
		rightSigs[sigf(rs.Metric)] = struct{}{}
	}

	for _, ls := range lhs {
		// If there's a matching entry in the right-hand side Vector, add the sample.
		if _, ok := rightSigs[sigf(ls.Metric)]; ok {
			enh.Out = append(enh.Out, ls)
		}
	}
	return enh.Out
}

func (ev *evaluator) VectorOr(lhs, rhs Vector, matching *parser.VectorMatching, enh *EvalNodeHelper) Vector {
	if matching.Card != parser.CardManyToMany {
		panic("set operations must only use many-to-many matching")
	}
	sigf := enh.signatureFunc(matching.On, matching.MatchingLabels...)

	leftSigs := map[string]struct{}{}
	// Add everything from the left-hand-side Vector.
	for _, ls := range lhs {
		leftSigs[sigf(ls.Metric)] = struct{}{}
		enh.Out = append(enh.Out, ls)
	}
	// Add all right-hand side elements which have not been added from the left-hand side.
	for _, rs := range rhs {
		if _, ok := leftSigs[sigf(rs.Metric)]; !ok {
			enh.Out = append(enh.Out, rs)
		}
	}
	return enh.Out
}

func (ev *evaluator) VectorUnless(lhs, rhs Vector, matching *parser.VectorMatching, enh *EvalNodeHelper) Vector {
	if matching.Card != parser.CardManyToMany {
		panic("set operations must only use many-to-many matching")
	}
	sigf := enh.signatureFunc(matching.On, matching.MatchingLabels...)

	rightSigs := map[string]struct{}{}
	for _, rs := range rhs {
		rightSigs[sigf(rs.Metric)] = struct{}{}
	}

	for _, ls := range lhs {
		if _, ok := rightSigs[sigf(ls.Metric)]; !ok {
			enh.Out = append(enh.Out, ls)
		}
	}
	return enh.Out
}

// VectorBinop evaluates a binary operation between two Vectors, excluding set operators.
func (ev *evaluator) VectorBinop(op parser.ItemType, lhs, rhs Vector, matching *parser.VectorMatching, returnBool bool, enh *EvalNodeHelper) Vector {
	if matching.Card == parser.CardManyToMany {
		panic("many-to-many only allowed for set operators")
	}
	sigf := enh.signatureFunc(matching.On, matching.MatchingLabels...)

	// The control flow below handles one-to-one or many-to-one matching.
	// For one-to-many, swap sidedness and account for the swap when calculating
	// values.
	if matching.Card == parser.CardOneToMany {
		lhs, rhs = rhs, lhs
	}

	// All samples from the rhs hashed by the matching label/values.
	if enh.rightSigs == nil {
		enh.rightSigs = make(map[string]Sample, len(enh.Out))
	} else {
		for k := range enh.rightSigs {
			delete(enh.rightSigs, k)
		}
	}
	rightSigs := enh.rightSigs

	// Add all rhs samples to a map so we can easily find matches later.
	for _, rs := range rhs {
		sig := sigf(rs.Metric)
		// The rhs is guaranteed to be the 'one' side. Having multiple samples
		// with the same signature means that the matching is many-to-many.
		if duplSample, found := rightSigs[sig]; found {
			// oneSide represents which side of the vector represents the 'one' in the many-to-one relationship.
			oneSide := "right"
			if matching.Card == parser.CardOneToMany {
				oneSide = "left"
			}
			matchedLabels := rs.Metric.MatchLabels(matching.On, matching.MatchingLabels...)
			// Many-to-many matching not allowed.
			ev.errorf("found duplicate series for the match group %s on the %s hand-side of the operation: [%s, %s]"+
				";many-to-many matching not allowed: matching labels must be unique on one side", matchedLabels.String(), oneSide, rs.Metric.String(), duplSample.Metric.String())
		}
		rightSigs[sig] = rs
	}

	// Tracks the match-signature. For one-to-one operations the value is nil. For many-to-one
	// the value is a set of signatures to detect duplicated result elements.
	if enh.matchedSigs == nil {
		enh.matchedSigs = make(map[string]map[uint64]struct{}, len(rightSigs))
	} else {
		for k := range enh.matchedSigs {
			delete(enh.matchedSigs, k)
		}
	}
	matchedSigs := enh.matchedSigs

	// For all lhs samples find a respective rhs sample and perform
	// the binary operation.
	for _, ls := range lhs {
		sig := sigf(ls.Metric)

		rs, found := rightSigs[sig] // Look for a match in the rhs Vector.
		if !found {
			continue
		}

		// Account for potentially swapped sidedness.
		vl, vr := ls.V, rs.V
		if matching.Card == parser.CardOneToMany {
			vl, vr = vr, vl
		}
		value, keep := vectorElemBinop(op, vl, vr)
		if returnBool {
			if keep {
				value = 1.0
			} else {
				value = 0.0
			}
		} else if !keep {
			continue
		}
		metric := resultMetric(ls.Metric, rs.Metric, op, matching, enh)
		if returnBool {
			metric = enh.DropMetricName(metric)
		}
		insertedSigs, exists := matchedSigs[sig]
		if matching.Card == parser.CardOneToOne {
			if exists {
				ev.errorf("multiple matches for labels: many-to-one matching must be explicit (group_left/group_right)")
			}
			matchedSigs[sig] = nil // Set existence to true.
		} else {
			// In many-to-one matching the grouping labels have to ensure a unique metric
			// for the result Vector. Check whether those labels have already been added for
			// the same matching labels.
			insertSig := metric.Hash()

			if !exists {
				insertedSigs = map[uint64]struct{}{}
				matchedSigs[sig] = insertedSigs
			} else if _, duplicate := insertedSigs[insertSig]; duplicate {
				ev.errorf("multiple matches for labels: grouping labels must ensure unique matches")
			}
			insertedSigs[insertSig] = struct{}{}
		}

		enh.Out = append(enh.Out, Sample{
			Metric: metric,
			Point:  Point{V: value},
		})
	}
	return enh.Out
}

// signatureFunc returns a function that calculates the signature for a metric
// ignoring the provided labels. If on, then the given labels are only used instead.
func signatureFunc(on bool, b []byte, names ...string) func(labels.Labels) string {
	sort.Strings(names)
	if on {
		return func(lset labels.Labels) string {
			return string(lset.WithLabels(names...).Bytes(b))
		}
	}
	return func(lset labels.Labels) string {
		return string(lset.WithoutLabels(names...).Bytes(b))
	}
}

// resultMetric returns the metric for the given sample(s) based on the Vector
// binary operation and the matching options.
func resultMetric(lhs, rhs labels.Labels, op parser.ItemType, matching *parser.VectorMatching, enh *EvalNodeHelper) labels.Labels {
	if enh.resultMetric == nil {
		enh.resultMetric = make(map[string]labels.Labels, len(enh.Out))
	}

	if enh.lb == nil {
		enh.lb = labels.NewBuilder(lhs)
	} else {
		enh.lb.Reset(lhs)
	}

	buf := bytes.NewBuffer(enh.lblResultBuf[:0])
	enh.lblBuf = lhs.Bytes(enh.lblBuf)
	buf.Write(enh.lblBuf)
	enh.lblBuf = rhs.Bytes(enh.lblBuf)
	buf.Write(enh.lblBuf)
	enh.lblResultBuf = buf.Bytes()

	if ret, ok := enh.resultMetric[string(enh.lblResultBuf)]; ok {
		return ret
	}
	str := string(enh.lblResultBuf)

	if shouldDropMetricName(op) {
		enh.lb.Del(labels.MetricName)
	}

	if matching.Card == parser.CardOneToOne {
		if matching.On {
		Outer:
			for _, l := range lhs {
				for _, n := range matching.MatchingLabels {
					if l.Name == n {
						continue Outer
					}
				}
				enh.lb.Del(l.Name)
			}
		} else {
			enh.lb.Del(matching.MatchingLabels...)
		}
	}
	for _, ln := range matching.Include {
		// Included labels from the `group_x` modifier are taken from the "one"-side.
		if v := rhs.Get(ln); v != "" {
			enh.lb.Set(ln, v)
		} else {
			enh.lb.Del(ln)
		}
	}

	ret := enh.lb.Labels()
	enh.resultMetric[str] = ret
	return ret
}

// VectorscalarBinop evaluates a binary operation between a Vector and a Scalar.
func (ev *evaluator) VectorscalarBinop(op parser.ItemType, lhs Vector, rhs Scalar, swap, returnBool bool, enh *EvalNodeHelper) Vector {
	for _, lhsSample := range lhs {
		lv, rv := lhsSample.V, rhs.V
		// lhs always contains the Vector. If the original position was different
		// swap for calculating the value.
		if swap {
			lv, rv = rv, lv
		}
		value, keep := vectorElemBinop(op, lv, rv)
		// Catch cases where the scalar is the LHS in a scalar-vector comparison operation.
		// We want to always keep the vector element value as the output value, even if it's on the RHS.
		if op.IsComparisonOperator() && swap {
			value = rv
		}
		if returnBool {
			if keep {
				value = 1.0
			} else {
				value = 0.0
			}
			keep = true
		}
		if keep {
			lhsSample.V = value
			if shouldDropMetricName(op) || returnBool {
				lhsSample.Metric = enh.DropMetricName(lhsSample.Metric)
			}
			enh.Out = append(enh.Out, lhsSample)
		}
	}
	return enh.Out
}

func dropMetricName(l labels.Labels) labels.Labels {
	return labels.NewBuilder(l).Del(labels.MetricName).Labels()
}

// scalarBinop evaluates a binary operation between two Scalars.
func scalarBinop(op parser.ItemType, lhs, rhs float64) float64 {
	switch op {
	case parser.ADD:
		return lhs + rhs
	case parser.SUB:
		return lhs - rhs
	case parser.MUL:
		return lhs * rhs
	case parser.DIV:
		return lhs / rhs
	case parser.POW:
		return math.Pow(lhs, rhs)
	case parser.MOD:
		return math.Mod(lhs, rhs)
	case parser.EQLC:
		return btos(lhs == rhs)
	case parser.NEQ:
		return btos(lhs != rhs)
	case parser.GTR:
		return btos(lhs > rhs)
	case parser.LSS:
		return btos(lhs < rhs)
	case parser.GTE:
		return btos(lhs >= rhs)
	case parser.LTE:
		return btos(lhs <= rhs)
	}
	panic(errors.Errorf("operator %q not allowed for Scalar operations", op))
}

// vectorElemBinop evaluates a binary operation between two Vector elements.
func vectorElemBinop(op parser.ItemType, lhs, rhs float64) (float64, bool) {
	switch op {
	case parser.ADD:
		return lhs + rhs, true
	case parser.SUB:
		return lhs - rhs, true
	case parser.MUL:
		return lhs * rhs, true
	case parser.DIV:
		return lhs / rhs, true
	case parser.POW:
		return math.Pow(lhs, rhs), true
	case parser.MOD:
		return math.Mod(lhs, rhs), true
	case parser.EQLC:
		return lhs, lhs == rhs
	case parser.NEQ:
		return lhs, lhs != rhs
	case parser.GTR:
		return lhs, lhs > rhs
	case parser.LSS:
		return lhs, lhs < rhs
	case parser.GTE:
		return lhs, lhs >= rhs
	case parser.LTE:
		return lhs, lhs <= rhs
	}
	panic(errors.Errorf("operator %q not allowed for operations between Vectors", op))
}

type groupedAggregation struct {
	labels      labels.Labels
	value       float64
	mean        float64
	groupCount  int
	heap        vectorByValueHeap
	reverseHeap vectorByReverseValueHeap
}

// aggregation evaluates an aggregation operation on a Vector. The provided grouping labels
// must be sorted.
func (ev *evaluator) aggregation(op parser.ItemType, grouping []string, without bool, param interface{}, vec Vector, seriesHelper []EvalSeriesHelper, enh *EvalNodeHelper) Vector {

	result := map[uint64]*groupedAggregation{}
	var k int64
	if op == parser.TOPK || op == parser.BOTTOMK {
		f := param.(float64)
		if !convertibleToInt64(f) {
			ev.errorf("Scalar value %v overflows int64", f)
		}
		k = int64(f)
		if k < 1 {
			return Vector{}
		}
	}
	var q float64
	if op == parser.QUANTILE {
		q = param.(float64)
	}
	var valueLabel string
	var recomputeGroupingKey bool
	if op == parser.COUNT_VALUES {
		valueLabel = param.(string)
		if !model.LabelName(valueLabel).IsValid() {
			ev.errorf("invalid label name %q", valueLabel)
		}
		if !without {
			// We're changing the grouping labels so we have to ensure they're still sorted
			// and we have to flag to recompute the grouping key. Considering the count_values()
			// operator is less frequently used than other aggregations, we're fine having to
			// re-compute the grouping key on each step for this case.
			grouping = append(grouping, valueLabel)
			sort.Strings(grouping)
			recomputeGroupingKey = true
		}
	}

	lb := labels.NewBuilder(nil)
	var buf []byte
	for si, s := range vec {
		metric := s.Metric

		if op == parser.COUNT_VALUES {
			lb.Reset(metric)
			lb.Set(valueLabel, strconv.FormatFloat(s.V, 'f', -1, 64))
			metric = lb.Labels()

			// We've changed the metric so we have to recompute the grouping key.
			recomputeGroupingKey = true
		}

		// We can use the pre-computed grouping key unless grouping labels have changed.
		var groupingKey uint64
		if !recomputeGroupingKey {
			groupingKey = seriesHelper[si].groupingKey
		} else {
			groupingKey, buf = generateGroupingKey(metric, grouping, without, buf)
		}

		group, ok := result[groupingKey]
		// Add a new group if it doesn't exist.
		if !ok {
			var m labels.Labels

			if without {
				lb.Reset(metric)
				lb.Del(grouping...)
				lb.Del(labels.MetricName)
				m = lb.Labels()
			} else {
				m = metric.WithLabels(grouping...)
			}
			result[groupingKey] = &groupedAggregation{
				labels:     m,
				value:      s.V,
				mean:       s.V,
				groupCount: 1,
			}
			inputVecLen := int64(len(vec))
			resultSize := k
			if k > inputVecLen {
				resultSize = inputVecLen
			}
			switch op {
			case parser.STDVAR, parser.STDDEV:
				result[groupingKey].value = 0
			case parser.TOPK, parser.QUANTILE:
				result[groupingKey].heap = make(vectorByValueHeap, 0, resultSize)
				heap.Push(&result[groupingKey].heap, &Sample{
					Point:  Point{V: s.V},
					Metric: s.Metric,
				})
			case parser.BOTTOMK:
				result[groupingKey].reverseHeap = make(vectorByReverseValueHeap, 0, resultSize)
				heap.Push(&result[groupingKey].reverseHeap, &Sample{
					Point:  Point{V: s.V},
					Metric: s.Metric,
				})
			case parser.GROUP:
				result[groupingKey].value = 1
			}
			continue
		}

		switch op {
		case parser.SUM:
			group.value += s.V

		case parser.AVG:
			group.groupCount++
			if math.IsInf(group.mean, 0) {
				if math.IsInf(s.V, 0) && (group.mean > 0) == (s.V > 0) {
					// The `mean` and `s.V` values are `Inf` of the same sign.  They
					// can't be subtracted, but the value of `mean` is correct
					// already.
					break
				}
				if !math.IsInf(s.V, 0) && !math.IsNaN(s.V) {
					// At this stage, the mean is an infinite. If the added
					// value is neither an Inf or a Nan, we can keep that mean
					// value.
					// This is required because our calculation below removes
					// the mean value, which would look like Inf += x - Inf and
					// end up as a NaN.
					break
				}
			}
			// Divide each side of the `-` by `group.groupCount` to avoid float64 overflows.
			group.mean += s.V/float64(group.groupCount) - group.mean/float64(group.groupCount)

		case parser.GROUP:
			// Do nothing. Required to avoid the panic in `default:` below.

		case parser.MAX:
			if group.value < s.V || math.IsNaN(group.value) {
				group.value = s.V
			}

		case parser.MIN:
			if group.value > s.V || math.IsNaN(group.value) {
				group.value = s.V
			}

		case parser.COUNT, parser.COUNT_VALUES:
			group.groupCount++

		case parser.STDVAR, parser.STDDEV:
			group.groupCount++
			delta := s.V - group.mean
			group.mean += delta / float64(group.groupCount)
			group.value += delta * (s.V - group.mean)

		case parser.TOPK:
			if int64(len(group.heap)) < k || group.heap[0].V < s.V || math.IsNaN(group.heap[0].V) {
				if int64(len(group.heap)) == k {
					heap.Pop(&group.heap)
				}
				heap.Push(&group.heap, &Sample{
					Point:  Point{V: s.V},
					Metric: s.Metric,
				})
			}

		case parser.BOTTOMK:
			if int64(len(group.reverseHeap)) < k || group.reverseHeap[0].V > s.V || math.IsNaN(group.reverseHeap[0].V) {
				if int64(len(group.reverseHeap)) == k {
					heap.Pop(&group.reverseHeap)
				}
				heap.Push(&group.reverseHeap, &Sample{
					Point:  Point{V: s.V},
					Metric: s.Metric,
				})
			}

		case parser.QUANTILE:
			group.heap = append(group.heap, s)

		default:
			panic(errors.Errorf("expected aggregation operator but got %q", op))
		}
	}

	// Construct the result Vector from the aggregated groups.
	for _, aggr := range result {
		switch op {
		case parser.AVG:
			aggr.value = aggr.mean

		case parser.COUNT, parser.COUNT_VALUES:
			aggr.value = float64(aggr.groupCount)

		case parser.STDVAR:
			aggr.value = aggr.value / float64(aggr.groupCount)

		case parser.STDDEV:
			aggr.value = math.Sqrt(aggr.value / float64(aggr.groupCount))

		case parser.TOPK:
			// The heap keeps the lowest value on top, so reverse it.
			sort.Sort(sort.Reverse(aggr.heap))
			for _, v := range aggr.heap {
				enh.Out = append(enh.Out, Sample{
					Metric: v.Metric,
					Point:  Point{V: v.V},
				})
			}
			continue // Bypass default append.

		case parser.BOTTOMK:
			// The heap keeps the highest value on top, so reverse it.
			sort.Sort(sort.Reverse(aggr.reverseHeap))
			for _, v := range aggr.reverseHeap {
				enh.Out = append(enh.Out, Sample{
					Metric: v.Metric,
					Point:  Point{V: v.V},
				})
			}
			continue // Bypass default append.

		case parser.QUANTILE:
			aggr.value = quantile(q, aggr.heap)

		default:
			// For other aggregations, we already have the right value.
		}

		enh.Out = append(enh.Out, Sample{
			Metric: aggr.labels,
			Point:  Point{V: aggr.value},
		})
	}
	return enh.Out
}

// groupingKey builds and returns the grouping key for the given metric and
// grouping labels.
func generateGroupingKey(metric labels.Labels, grouping []string, without bool, buf []byte) (uint64, []byte) {
	if without {
		return metric.HashWithoutLabels(buf, grouping...)
	}

	if len(grouping) == 0 {
		// No need to generate any hash if there are no grouping labels.
		return 0, buf
	}

	return metric.HashForLabels(buf, grouping...)
}

// btos returns 1 if b is true, 0 otherwise.
func btos(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// shouldDropMetricName returns whether the metric name should be dropped in the
// result of the op operation.
func shouldDropMetricName(op parser.ItemType) bool {
	switch op {
	case parser.ADD, parser.SUB, parser.DIV, parser.MUL, parser.POW, parser.MOD:
		return true
	default:
		return false
	}
}

// NewOriginContext returns a new context with data about the origin attached.
func NewOriginContext(ctx context.Context, data map[string]interface{}) context.Context {
	return context.WithValue(ctx, queryOrigin{}, data)
}

func formatDate(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// unwrapParenExpr does the AST equivalent of removing parentheses around a expression.
func unwrapParenExpr(e *parser.Expr) {
	for {
		if p, ok := (*e).(*parser.ParenExpr); ok {
			*e = p.Expr
		} else {
			break
		}
	}
}

func unwrapStepInvariantExpr(e parser.Expr) parser.Expr {
	if p, ok := e.(*parser.StepInvariantExpr); ok {
		return p.Expr
	}
	return e
}

// PreprocessExpr wraps all possible step invariant parts of the given expression with
// StepInvariantExpr. It also resolves the preprocessors.
func PreprocessExpr(expr parser.Expr, start, end time.Time) parser.Expr {
	isStepInvariant := preprocessExprHelper(expr, start, end)
	if isStepInvariant {
		return newStepInvariantExpr(expr)
	}
	return expr
}

// preprocessExprHelper wraps the child nodes of the expression
// with a StepInvariantExpr wherever it's step invariant. The returned boolean is true if the
// passed expression qualifies to be wrapped by StepInvariantExpr.
// It also resolves the preprocessors.
func preprocessExprHelper(expr parser.Expr, start, end time.Time) bool {
	switch n := expr.(type) {
	case *parser.VectorSelector:
		if n.StartOrEnd == parser.START {
			n.Timestamp = makeInt64Pointer(timestamp.FromTime(start))
		} else if n.StartOrEnd == parser.END {
			n.Timestamp = makeInt64Pointer(timestamp.FromTime(end))
		}
		return n.Timestamp != nil

	case *parser.AggregateExpr:
		return preprocessExprHelper(n.Expr, start, end)

	case *parser.BinaryExpr:
		isInvariant1, isInvariant2 := preprocessExprHelper(n.LHS, start, end), preprocessExprHelper(n.RHS, start, end)
		if isInvariant1 && isInvariant2 {
			return true
		}

		if isInvariant1 {
			n.LHS = newStepInvariantExpr(n.LHS)
		}
		if isInvariant2 {
			n.RHS = newStepInvariantExpr(n.RHS)
		}

		return false

	case *parser.Call:
		_, ok := AtModifierUnsafeFunctions[n.Func.Name]
		isStepInvariant := !ok
		isStepInvariantSlice := make([]bool, len(n.Args))
		for i := range n.Args {
			isStepInvariantSlice[i] = preprocessExprHelper(n.Args[i], start, end)
			isStepInvariant = isStepInvariant && isStepInvariantSlice[i]
		}

		if isStepInvariant {

			// The function and all arguments are step invariant.
			return true
		}

		for i, isi := range isStepInvariantSlice {
			if isi {
				n.Args[i] = newStepInvariantExpr(n.Args[i])
			}
		}
		return false

	case *parser.MatrixSelector:
		return preprocessExprHelper(n.VectorSelector, start, end)

	case *parser.SubqueryExpr:
		// Since we adjust offset for the @ modifier evaluation,
		// it gets tricky to adjust it for every subquery step.
		// Hence we wrap the inside of subquery irrespective of
		// @ on subquery (given it is also step invariant) so that
		// it is evaluated only once w.r.t. the start time of subquery.
		isInvariant := preprocessExprHelper(n.Expr, start, end)
		if isInvariant {
			n.Expr = newStepInvariantExpr(n.Expr)
		}
		if n.StartOrEnd == parser.START {
			n.Timestamp = makeInt64Pointer(timestamp.FromTime(start))
		} else if n.StartOrEnd == parser.END {
			n.Timestamp = makeInt64Pointer(timestamp.FromTime(end))
		}
		return n.Timestamp != nil

	case *parser.ParenExpr:
		return preprocessExprHelper(n.Expr, start, end)

	case *parser.UnaryExpr:
		return preprocessExprHelper(n.Expr, start, end)

	case *parser.StringLiteral, *parser.NumberLiteral:
		return true
	}

	panic(fmt.Sprintf("found unexpected node %#v", expr))
}

func newStepInvariantExpr(expr parser.Expr) parser.Expr {
	if e, ok := expr.(*parser.ParenExpr); ok {
		// Wrapping the inside of () makes it easy to unwrap the paren later.
		// But this effectively unwraps the paren.
		return newStepInvariantExpr(e.Expr)

	}
	return &parser.StepInvariantExpr{Expr: expr}
}

// setOffsetForAtModifier modifies the offset of vector and matrix selector
// and subquery in the tree to accommodate the timestamp of @ modifier.
// The offset is adjusted w.r.t. the given evaluation time.
func setOffsetForAtModifier(evalTime int64, expr parser.Expr) {
	getOffset := func(ts *int64, originalOffset time.Duration, path []parser.Node) time.Duration {
		if ts == nil {
			return originalOffset
		}

		subqOffset, _, subqTs := subqueryTimes(path)
		if subqTs != nil {
			subqOffset += time.Duration(evalTime-*subqTs) * time.Millisecond
		}

		offsetForTs := time.Duration(evalTime-*ts) * time.Millisecond
		offsetDiff := offsetForTs - subqOffset
		return originalOffset + offsetDiff
	}

	parser.Inspect(expr, func(node parser.Node, path []parser.Node) error {
		switch n := node.(type) {
		case *parser.VectorSelector:
			n.Offset = getOffset(n.Timestamp, n.OriginalOffset, path)

		case *parser.MatrixSelector:
			vs := n.VectorSelector.(*parser.VectorSelector)
			vs.Offset = getOffset(vs.Timestamp, vs.OriginalOffset, path)

		case *parser.SubqueryExpr:
			n.Offset = getOffset(n.Timestamp, n.OriginalOffset, path)
		}
		return nil
	})
}

func makeInt64Pointer(val int64) *int64 {
	valp := new(int64)
	*valp = val
	return valp
}
