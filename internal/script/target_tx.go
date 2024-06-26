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

package script

import (
	"context"
	"database/sql"
	"sync"

	"github.com/cockroachdb/replicator/internal/types"
	"github.com/dop251/goja"
	"github.com/pkg/errors"
)

// targetTX is a facade passed to the userscript to expose the target
// database transaction and various other metadata.
type targetTX struct {
	*applier

	batchTemplate *types.TableBatch   // Used by Apply for metadata.
	ctx           context.Context     // Passed to database methods.
	columns       []map[string]any    // Lazily-constructed schema data.
	tq            types.TargetQuerier // The database transaction.
	mu            sync.Mutex          // Serializes access to methods on tq.
}

var _ asyncTracker = (*targetTX)(nil)

// Apply may be called by the user script to use the default
// [types.TableAcceptor] for continued processing. This enables uses
// cases where the user script acts as an interceptor.
func (tx *targetTX) Apply(ops []*applyOp) *goja.Promise {
	return tx.parent.execTrackedPromise(tx, func(resolve func(result any)) error {
		// Convert the operations back into standard mutations.
		batch := tx.batchTemplate.Copy()
		if err := opsIntoBatch(tx.ctx, ops, batch); err != nil {
			return err
		}
		// Use the standard TableAcceptor to process the batch.
		if err := tx.parent.Delegate.AcceptTableBatch(tx.ctx, batch, &types.AcceptOptions{
			TargetQuerier: tx.tq,
		}); err != nil {
			return err
		}
		resolve(goja.Undefined())
		return nil
	})
}

// Columns is exported to the userscript. It will lazily populate the
// columns field.
func (tx *targetTX) Columns() []map[string]any {
	if len(tx.columns) > 0 {
		return tx.columns
	}
	cols := tx.parent.watcher.Get().Columns.GetZero(tx.table)
	for _, col := range cols {
		// Keep in sync with .d.ts file.
		m := map[string]any{
			"ignored": col.Ignored,
			"name":    col.Name.String(),
			"primary": col.Primary,
			"type":    col.Type,
		}
		// It's JS-idiomatic for the string to be null than empty.
		if col.DefaultExpr != "" {
			m["defaultExpr"] = col.DefaultExpr
		}
		tx.columns = append(tx.columns, m)
	}
	return tx.columns
}

// Enter implements [asyncTracker]. It will inject the targetTX into the
// runtime so the user code may use it.
func (tx *targetTX) enter(script *UserScript) error {
	return script.apiModule.Set("getTX", func() *targetTX {
		return tx
	})
}

// Exec is exported to the userscript.
func (tx *targetTX) Exec(q string, args ...any) *goja.Promise {
	return tx.parent.execTrackedPromise(tx, func(resolve func(result any)) error {
		tx.mu.Lock()
		defer tx.mu.Unlock()
		if _, err := tx.tq.ExecContext(tx.ctx, q, args...); err != nil {
			return errors.Wrap(err, q)
		}
		resolve(goja.Undefined())
		return nil
	})
}

// Exit implements [asyncTracker]. It will clean up the references set
// by [targetTX.enter].
func (tx *targetTX) exit(script *UserScript) error {
	return script.apiModule.Set("getTX", notInTransaction)
}

// Query is exported to the userscript.
func (tx *targetTX) Query(q string, args ...any) *goja.Promise {
	// Pre-construct the iterator JS object by setting
	// Symbol.iterator to a function that returns a value which
	// implements the iterator protocol (i.e. has a next()
	// function). We want to do this while we're being called from JS.
	obj := tx.parent.rt.NewObject()
	iterator := &rowsIter{rt: tx.parent.rt}
	if err := obj.SetSymbol(goja.SymIterator, func() *rowsIter {
		return iterator
	}); err != nil {
		failed, _, rejected := tx.parent.rt.NewPromise()
		rejected(errors.WithStack(err))
		return failed
	}

	// Execute the SQL in a (pooled) background goroutine.
	return tx.parent.execTrackedPromise(tx, func(resolve func(any)) error {
		tx.mu.Lock()
		defer tx.mu.Unlock()
		rows, err := tx.tq.QueryContext(tx.ctx, q, args...)
		if err != nil {
			return errors.Wrap(err, q)
		}
		defer func() { _ = rows.Close() }()

		iterator.rows = rows

		// Extract the number of columns for the result iterator.
		if names, err := rows.Columns(); err == nil {
			iterator.colCount = len(names)
		} else {
			return errors.Wrap(err, q)
		}

		resolve(obj)
		return nil
	})
}

// Schema is exported to the userscript.
func (tx *targetTX) Schema() string {
	return tx.table.Schema().String()
}

// Table is exported to the userscript.
func (tx *targetTX) Table() string {
	return tx.table.String()
}

// rowsIter exports a [sql.Rows] into a JS API that conforms to the
// iterator protocol. Note that goja does not (as of this writing)
// support the async iterable protocol.
//
// https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Iteration_protocols
// https://pkg.go.dev/github.com/dop251/goja#example-Object.SetSymbol
type rowsIter struct {
	colCount int
	rows     *sql.Rows
	rt       *goja.Runtime
}

// Next implements the JS iterator protocol.
func (it *rowsIter) Next() (*rowsIterResult, error) {
	next := it.rows.Next()
	err := it.rows.Err()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if !next {
		return &rowsIterResult{Done: true}, nil
	}

	rawValues := make([]any, it.colCount)
	ptrs := make([]any, len(rawValues))
	for idx := range ptrs {
		ptrs[idx] = &rawValues[idx]
	}
	if err := it.rows.Scan(ptrs...); err != nil {
		return nil, errors.WithStack(err)
	}
	// We want to present database types in a manner consistent with how
	// they would be seen if received from a changefeed.
	values := make([]goja.Value, len(rawValues))
	for idx, rawValue := range rawValues {
		values[idx], err = safeValue(it.rt, rawValue)
		if err != nil {
			return nil, err
		}
	}
	return &rowsIterResult{Value: values}, nil
}

// Return implements the JS iterator protocol and will be called
// if the iterator is not being read to completion. This allows us
// to preemptively close the rowset.
func (it *rowsIter) Return() *rowsIterResult {
	_ = it.rows.Close()
	return &rowsIterResult{Done: true}
}

// Implements the JS iteration result protocol.
type rowsIterResult struct {
	Done  bool         `goja:"done"`
	Value []goja.Value `goja:"value"`
}
