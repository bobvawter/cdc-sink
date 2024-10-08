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

// Package besteffort relaxes the consistency of a target schema.
package besteffort

import (
	"context"
	"time"

	"github.com/cockroachdb/field-eng-powertools/notify"
	"github.com/cockroachdb/field-eng-powertools/stopper"
	"github.com/cockroachdb/field-eng-powertools/stopvar"
	"github.com/cockroachdb/replicator/internal/sequencer"
	"github.com/cockroachdb/replicator/internal/sequencer/scheduler"
	"github.com/cockroachdb/replicator/internal/types"
	"github.com/cockroachdb/replicator/internal/util/hlc"
	"github.com/cockroachdb/replicator/internal/util/ident"
	log "github.com/sirupsen/logrus"
)

// BestEffort relaxes the overall consistency of a target schema to
// improve throughput for smaller groups of tables defined by
// foreign-key relationships.
type BestEffort struct {
	cfg         *sequencer.Config
	scheduler   *scheduler.Scheduler
	stagers     types.Stagers
	stagingPool *types.StagingPool
	timeSource  func() hlc.Time
	watchers    types.Watchers
}

var _ sequencer.Shim = (*BestEffort)(nil)

// SetTimeSource is called by tests that need to ensure lock-step
// behaviors in sweepTable or when testing the proactive timestamp
// behavior.
func (s *BestEffort) SetTimeSource(source func() hlc.Time) {
	s.timeSource = source
}

// Wrap implements [sequencer.Shim].
func (s *BestEffort) Wrap(
	_ *stopper.Context, delegate sequencer.Sequencer,
) (sequencer.Sequencer, error) {
	return &bestEffort{
		BestEffort: s,
		delegate:   delegate,
	}, nil
}

type bestEffort struct {
	*BestEffort

	delegate sequencer.Sequencer

	schemaChanged *notify.Var[struct{}] // Used by tests.
}

var _ sequencer.Sequencer = (*bestEffort)(nil)

// SchemaChanged is called by test code before starting.
func (s *bestEffort) SchemaChanged() *notify.Var[struct{}] {
	if s.schemaChanged == nil {
		s.schemaChanged = notify.VarOf(struct{}{})
	}
	return s.schemaChanged
}

// Start implements [sequencer.Starter]. It will start multiple
// instances of the delegate sequencer, once for each
// referentially-connected group of tables.
func (s *bestEffort) Start(
	ctx *stopper.Context, opts *sequencer.StartOptions,
) (types.MultiAcceptor, *notify.Var[sequencer.Stat], error) {
	watcher, err := s.watchers.Get(opts.Group.Enclosing)
	if err != nil {
		return nil, nil, err
	}

	// Generate a synthetic maximum checkpoint bound in the absence
	// of any existing checkpoints. This allows partial progress to
	// be made in advance of receiving any checkpoints from the
	// source.
	ctx.Go(func(ctx *stopper.Context) error {
		for {
			if _, _, err := opts.Bounds.Update(func(old hlc.Range) (hlc.Range, error) {
				// Cancel this task once there are checkpoints.
				if old.Min() != hlc.Zero() {
					return hlc.Range{}, context.Canceled
				}
				// This source has a negative offset from the
				// current time. If there's a single, unapplied
				// checkpoint, it should be in the relative future
				// from the synthetic ones.
				proposed := s.timeSource()
				if hlc.Compare(proposed, old.MaxInclusive()) > 0 {
					return hlc.RangeIncluding(old.Min(), proposed), nil
				}
				return hlc.Range{}, notify.ErrNoUpdate
			}); err != nil {
				// Will be context.Canceled from callback above.
				return nil
			}
			select {
			case <-time.After(time.Second):
			case <-ctx.Stopping():
				return nil
			}
		}
	})

	// Ensure the initial map has all tables in it. This ensures that
	// all tables must make some progress before the stat will advance.
	statMap := &ident.TableMap[hlc.Range]{}
	for _, table := range opts.Group.Tables {
		statMap.Put(table, hlc.RangeEmpty())
	}
	stats := notify.VarOf(sequencer.NewStat(opts.Group, statMap))

	// Create an initial generation of sub-sequencers.
	schemaData := watcher.Get()
	cfg, err := s.startGeneration(ctx, opts, schemaData, stats)
	if err != nil {
		return nil, nil, err
	}

	// Start a process to keep the router's configuration updated
	// whenever there's a schema change. When the schema changes, we
	// want to start a new collection of sub-sequencers, swap the router
	// configuration, and then put the old generation into shutdown.
	ret := &router{config: notify.VarOf(cfg)}
	ctx.Go(func(ctx *stopper.Context) error {
		_, err := stopvar.DoWhenChanged(ctx, schemaData, watcher.GetNotify(),
			func(ctx *stopper.Context, _, schemaData *types.SchemaData) error {
				cfg, err := s.startGeneration(ctx, opts, schemaData, stats)
				if err != nil {
					log.WithError(err).Warn("could not create new BestEffort sequencers")
					return nil
				}
				oldCfg, _ := ret.config.Swap(cfg)
				// Notify test code.
				if s.schemaChanged != nil {
					s.schemaChanged.Notify()
				}
				log.Debug("reconfigured BestEffort due to schema change")
				if err := oldCfg.shutdown(); err != nil {
					log.WithError(err).Warn("error while shutting down previous BestEffort")
				}
				return nil
			})
		return err
	})

	return ret, stats, nil
}

// startGeneration creates the delegate sequences and returns a routing
// configuration to map incoming requests. The delegates will execute
// with a nested stopper.
func (s *bestEffort) startGeneration(
	ctx *stopper.Context,
	opts *sequencer.StartOptions,
	schemaData *types.SchemaData,
	stats *notify.Var[sequencer.Stat],
) (*routerConfig, error) {
	// Create a nested context.
	ctx = stopper.WithContext(ctx)

	cfg := &routerConfig{
		routes:     make(map[*types.SchemaComponent]types.MultiAcceptor),
		schemaData: schemaData,
		shutdown: func() error {
			ctx.Stop(s.cfg.TaskGracePeriod)
			return ctx.Wait()
		},
	}

	// Start a delegate sequencer for each non-overlapping subgroup of
	// tables in the target schema. This ensures that tables with FK
	// relationships can be swept in a coordinated fashion.
	for _, comp := range schemaData.Components {
		subOpts := opts.Copy()
		subOpts.Group.Tables = comp.Order
		subOpts.MaxDeferred = s.cfg.TimestampLimit

		subAcc, subStats, err := s.delegate.Start(ctx, subOpts)
		if err != nil {
			log.WithError(err).Warnf(
				"BestEffort.Start: could not start nested Sequencer for %s", comp.Order)
			return nil, err
		}

		// This is a special case for single-table groups, where
		// we'll try to write directly to the target table, rather
		// than wait for an entire stage-apply cycle.
		if len(comp.Order) == 1 {
			log.Tracef("enabling direct path for %s", comp.Order[0])
			subAcc = &directAcceptor{
				BestEffort: s.BestEffort,
				apply:      subOpts.Delegate,
				fallback:   subAcc,
			}
		}

		// Route incoming mutations to the component's sequencer.
		cfg.routes[comp] = subAcc

		// Start a helper to aggregate the progress values together.
		ctx.Go(func(ctx *stopper.Context) error {
			// Ignoring error since innermost callback returns nil.
			_, _ = stopvar.DoWhenChanged(ctx, nil, subStats, func(ctx *stopper.Context, _, subStat sequencer.Stat) error {
				_, _, err := stats.Update(func(old sequencer.Stat) (sequencer.Stat, error) {
					next := old.Copy()
					subStat.Progress().CopyInto(next.Progress())
					if log.IsLevelEnabled(log.TraceLevel) {
						buf, _ := next.Progress().MarshalJSON()
						log.Tracef("aggregated progress for group %s: %s",
							next.Group().Name, buf)
					}
					return next, nil
				})
				return err
			})
			return nil
		})
	}

	return cfg, nil
}
