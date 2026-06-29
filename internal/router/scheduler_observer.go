package router

import (
	"context"
	"time"

	"github.com/inference-book/inference-plane/internal/metrics"
)

// metricsObserver bridges scheduler.Observer to the metrics
// Recorder. The scheduler stays decoupled from internal/metrics by
// accepting the Observer interface; this adapter lives in the
// router because it's the layer that owns both the scheduler and
// the recorder.
//
// Push/Pop emissions tag with (deploy_id, tenant_id, priority) so
// the dashboard's per-class p95 panel + per-tenant depth panel
// have the labels they need.
//
// Push doesn't know deploy_id (the scheduler's observer signature
// only carries lane + tenant). For depth-gauge purposes, deploy_id
// is implicit in the queue's per-deployment routing once #84+
// adds multi-deployment fan-out; in v0.2 ch7-beat2.5 the router
// has one deployment per (model, replicaset) so the gauge cardinality
// is bounded by (lane × tenant × deploy_id) all the same. To keep
// the Push signal informative we emit it with deploy_id="" (the
// "unknown / cross-deployment" sentinel); the per-deployment depth
// is reconstructable from the Pop path which DOES carry deploy_id.
type metricsObserver struct {
	recorder *metrics.Recorder
}

func newMetricsObserver(recorder *metrics.Recorder) *metricsObserver {
	return &metricsObserver{recorder: recorder}
}

// OnPush records the post-push depth for (lane, tenant). deploy_id
// is unset here -- scheduler.Observer doesn't carry it on push;
// the dashboard's per-deployment depth panel keys on the Pop path's
// observation. nil-safe.
func (o *metricsObserver) OnPush(lane, tenantID string, depth int) {
	if o == nil || o.recorder == nil {
		return
	}
	o.recorder.RecordQueueDepth(context.Background(), "", tenantID, lane, int64(depth))
}

// OnPop records the post-pop depth + wait duration for
// (lane, tenant, deploy_id). Wait duration is 0 when the entry
// didn't carry an enqueue timestamp (test no-op entries); the
// recorder drops those.
func (o *metricsObserver) OnPop(lane, tenantID, deploymentID string, depth int, waitDur time.Duration) {
	if o == nil || o.recorder == nil {
		return
	}
	ctx := context.Background()
	o.recorder.RecordQueueDepth(ctx, deploymentID, tenantID, lane, int64(depth))
	o.recorder.RecordQueueWait(ctx, deploymentID, tenantID, lane, waitDur.Seconds())
}
