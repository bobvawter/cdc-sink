// Copyright 2024 The Cockroach Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

// Package besteffort contains a best-effort implementation of [types.MultiAcceptor].
package besteffort

import (
	"context"
	"math"
	"time"

	"github.com/cockroachdb/cdc-sink/internal/sequencer"
	"github.com/cockroachdb/cdc-sink/internal/sequencer/sequtil"
	"github.com/cockroachdb/cdc-sink/internal/types"
	"github.com/cockroachdb/cdc-sink/internal/util/hlc"
	"github.com/cockroachdb/cdc-sink/internal/util/ident"
	"github.com/cockroachdb/cdc-sink/internal/util/metrics"
	"github.com/cockroachdb/cdc-sink/internal/util/notify"
	"github.com/cockroachdb/cdc-sink/internal/util/stopper"
	"github.com/cockroachdb/cdc-sink/internal/util/stopvar"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sijms/go-ora/v2/network"
	log "github.com/sirupsen/logrus"
)

// BestEffort looks for deferred mutations and attempts to apply them on a
// best-effort basis. Mutations to apply are marked with a lease to
// ensure an at-least-once behavior.
type BestEffort struct {
	cfg         *sequencer.Config
	leases      types.Leases
	stagingPool *types.StagingPool
	stagers     types.Stagers
	targetPool  *types.TargetPool
	watchers    types.Watchers
}

var _ sequencer.Sequencer = (*BestEffort)(nil)

// Start implements [sequencer.Starter]. It will launch a background
// goroutine to attempt to apply staged mutations for each table within
// the group.
func (s *BestEffort) Start(
	ctx *stopper.Context, opts *sequencer.StartOptions,
) (types.MultiAcceptor, *notify.Var[sequencer.Stat], error) {
	stats := &notify.Var[sequencer.Stat]{}
	stats.Set(newStat(opts.Group))

	delegate := opts.Delegate

	// Identify which table's we're working on.
	gauges := make([]prometheus.Gauge, len(opts.Group.Tables))
	for idx, tbl := range opts.Group.Tables {
		gauges[idx] = sweepActive.WithLabelValues(metrics.TableValues(tbl)...)
	}

	// Acquire a lease on the group name to prevent multiple sweepers
	// from operating. In the future, this lease could be moved down
	// into the per-table method to allow multiple cdc-sink instances to
	// process different tables.
	//
	// https://github.com/cockroachdb/cdc-sink/issues/687
	sequtil.LeaseGroup(ctx, s.leases, opts.Group, func(ctx *stopper.Context, group *types.TableGroup) {
		// Launch a goroutine to handle sweeping for each table in the group.
		for idx, table := range group.Tables {
			idx, table := idx, table // Capture.
			ctx.Go(func() error {
				log.Tracef("BestEffort starting on %s", table)

				gauges[idx].Set(1)
				defer gauges[idx].Set(0)

				s.sweepTable(ctx, table, opts.Bounds, stats, delegate)
				log.Tracef("BestEffort stopping on %s", table)
				return nil
			})
		}
		// We want to hold the lease until we're shut down.
		<-ctx.Stopping()
	})

	// Respect table-dependency ordering.
	acc := types.OrderedAcceptorFrom(&acceptor{s, delegate}, s.watchers)

	return acc, stats, nil
}

// sweepTable implements the per-table loop behavior and communicates
// progress via the stats argument. It is blocking and should be called
// from a stoppered goroutine.
func (s *BestEffort) sweepTable(
	ctx *stopper.Context,
	table ident.Table,
	bounds *notify.Var[hlc.Range],
	stats *notify.Var[sequencer.Stat],
	acceptor types.TableAcceptor,
) {
	_, _ = stopvar.DoWhenChangedOrInterval(ctx, hlc.Range{}, bounds, s.cfg.QuiescentPeriod,
		func(ctx *stopper.Context, _, bound hlc.Range) error {
			// The bounds will be empty in the idle condition.
			if bound.Empty() {
				return nil
			}
			tableStat, err := s.sweepOnce(ctx, table, bound, acceptor)
			if err != nil {
				// We'll sleep below and then retry with the same or similar bounds.
				log.WithError(err).Warnf("BestEffort: error while sweeping table %s; will continue", table)
				return nil
			}
			// Ignoring error since callback returns nil.
			_, _, _ = stats.Update(func(old sequencer.Stat) (sequencer.Stat, error) {
				nextImpl := old.(*Stat).copy()
				nextImpl.Attempted += tableStat.Attempted
				nextImpl.Applied += tableStat.Applied
				nextImpl.Progress().Put(tableStat.LastTable, tableStat.LastTime)
				return nextImpl, nil
			})
			return nil
		})
}

// Stat is emitted by [BestEffort]. This is used by test code to inspect
// the (partial) progress that may be made.
type Stat struct {
	sequencer.Stat

	Applied   int         // The number of mutations that were actually applied.
	Attempted int         // The number of mutations that were seen.
	LastTable ident.Table // The table most recently processed.
	LastTime  hlc.Time    // The time that the table arrived at.
}

// newStat returns an initialized Stat.
func newStat(group *types.TableGroup) *Stat {
	return &Stat{
		Stat: sequencer.NewStat(group, &ident.TableMap[hlc.Time]{}),
	}
}

// Copy implements [sequencer.Stat].
func (s *Stat) Copy() sequencer.Stat { return s.copy() }

// copy creates a deep copy of the Stat.
func (s *Stat) copy() *Stat {
	next := *s
	next.Stat = next.Stat.Copy()
	return &next
}

// sweepOnce will execute a single pass for deferred, un-leased
// mutations within the time range.
func (s *BestEffort) sweepOnce(
	ctx *stopper.Context, tbl ident.Table, bounds hlc.Range, acceptor types.TableAcceptor,
) (*Stat, error) {
	start := time.Now()
	tblValues := metrics.TableValues(tbl)
	deferrals := sweepDeferrals.WithLabelValues(tblValues...)
	duration := sweepDuration.WithLabelValues(tblValues...)
	errCount := sweepErrors.WithLabelValues(tblValues...)
	sweepAttempted := sweepAttemptedCount.WithLabelValues(tblValues...)
	sweepApplied := sweepAppliedCount.WithLabelValues(tblValues...)
	sweepLastAttempt.WithLabelValues(tblValues...).SetToCurrentTime()

	log.Tracef("BestEffort.sweepOnce: starting %s", tbl)
	marker, err := s.stagers.Get(ctx, tbl)
	if err != nil {
		return nil, err
	}
	stat := &Stat{LastTable: tbl}
	q := &types.UnstageCursor{
		StartAt:        bounds.Min(),
		EndBefore:      bounds.Max(),
		Targets:        []ident.Table{tbl},
		TimestampLimit: math.MaxInt32,
		UpdateLimit:    s.cfg.SweepLimit,
	}
	for hasMore := true; hasMore && !ctx.IsStopping(); {
		// Reserve the mutation for a period of time. This will create
		// an upper bound on the rate at which any given mutation is
		// attempted.
		q.LeaseExpiry = time.Now().Add(s.cfg.QuiescentPeriod)

		// Collect some number of mutations.
		var pending []types.Mutation
		q, hasMore, err = s.stagers.Unstage(ctx, s.stagingPool, q,
			func(ctx context.Context, tbl ident.Table, mut types.Mutation) error {
				pending = append(pending, mut)
				return nil
			})
		if err != nil {
			return nil, err
		}
		stat.Attempted += len(pending)

		// This is a filter-in-place operation to retain only those
		// mutations that were successfully applied and which we can
		// subsequently mark as applied.
		successIdx := 0
		for _, mut := range pending {
			// We know that any mutation we're operating on has been
			// deferred at least once, so using larger batches is
			// unlikely to yield any real improvement here.
			batch := &types.TableBatch{
				Data:  []types.Mutation{mut},
				Table: tbl,
				Time:  mut.Time,
			}
			err := acceptor.AcceptTableBatch(ctx, batch, &types.AcceptOptions{})
			if err == nil {
				// Save applied mutations.
				pending[successIdx] = mut
				successIdx++
				continue
			}
			// Ignore deferrable errors, we'll try again later. We'll
			// leave the lease intact so that we might skip the mutation
			// until a later cycle.
			if isDeferrableError(err) {
				deferrals.Inc()
				continue
			}
			// Log any other errors, they're not going to block us and
			// we can retry later. We'll leave the lease expiration
			// intact so that we can skip this row for a period of time.
			log.WithError(err).Warnf(
				"sweep: table %s; key %s; will retry mutation later",
				tbl, string(mut.Key))
			errCount.Inc()
			continue
		}
		// Nothing changed, just return.
		if successIdx == 0 {
			break
		}
		// Mark the mutations as having been applied.
		if err := marker.MarkApplied(ctx, s.stagingPool, pending[:successIdx]); err != nil {
			return nil, err
		}
		stat.Applied += successIdx
	}

	log.Tracef("BestEffort.sweepOnce: completed %s (%d applied of %d attempted)",
		tbl, stat.Applied, stat.Attempted)
	stat.LastTime = q.EndBefore
	duration.Observe(time.Since(start).Seconds())
	sweepAttempted.Add(float64(stat.Attempted))
	sweepApplied.Add(float64(stat.Applied))
	return stat, nil
}

// isDeferrableError returns true if the error represents a temporary
// error which implies that the operation may succeed in the future
// (e.g. FK constraints).
//
// https://github.com/cockroachdb/cdc-sink/issues/688
func isDeferrableError(err error) bool {
	if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) {
		return pgErr.Code == "23503" // foreign_key_violation
	} else if myErr := (*mysql.MySQLError)(nil); errors.As(err, &myErr) {
		// Cannot add or update a child row: a foreign key constraint fails
		return myErr.Number == 1452
	} else if oraErr := (*network.OracleError)(nil); errors.As(err, &oraErr) {
		switch oraErr.ErrCode {
		case 1: // ORA-0001 unique constraint violated
			// The MERGE that we execute uses read-committed reads, so
			// it's possible for two concurrent merges to attempt to
			// insert the same row.
			return true
		case 2291: // ORA-02291: integrity constraint
			return true
		}
	}
	return false
}