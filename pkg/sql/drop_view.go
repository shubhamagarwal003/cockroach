// Copyright 2017 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package sql

import (
	"context"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/config"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/pkg/errors"
)

type dropViewNode struct {
	n  *tree.DropView
	td []*sqlbase.TableDescriptor
}

// DropView drops a view.
// Privileges: DROP on view.
//   Notes: postgres allows only the view owner to DROP a view.
//          mysql requires the DROP privilege on the view.
func (p *planner) DropView(ctx context.Context, n *tree.DropView) (planNode, error) {
	td := make([]*sqlbase.TableDescriptor, 0, len(n.Names))
	for _, name := range n.Names {
		tn, err := name.NormalizeTableName()
		if err != nil {
			return nil, err
		}
		if err := tn.QualifyWithDatabase(p.session.Database); err != nil {
			return nil, err
		}

		droppedDesc, err := p.dropTableOrViewPrepare(ctx, tn)
		if err != nil {
			return nil, err
		}
		if droppedDesc == nil {
			if n.IfExists {
				continue
			}
			// View does not exist, but we want it to: error out.
			return nil, sqlbase.NewUndefinedRelationError(tn)
		}
		if !droppedDesc.IsView() {
			return nil, sqlbase.NewWrongObjectTypeError(tn, "view")
		}

		td = append(td, droppedDesc)
	}

	// Ensure this view isn't depended on by any other views, or that if it is
	// then `cascade` was specified or it was also explicitly specified in the
	// DROP VIEW command.
	for _, droppedDesc := range td {
		for _, ref := range droppedDesc.DependedOnBy {
			// Don't verify that we can remove a dependent view if that dependent
			// view was explicitly specified in the DROP VIEW command.
			if descInSlice(ref.ID, td) {
				continue
			}
			if err := p.canRemoveDependentView(ctx, droppedDesc, ref, n.DropBehavior); err != nil {
				return nil, err
			}
		}
	}

	if len(td) == 0 {
		return &zeroNode{}, nil
	}
	return &dropViewNode{n: n, td: td}, nil
}

func (n *dropViewNode) startExec(params runParams) error {
	ctx := params.ctx
	for _, droppedDesc := range n.td {
		if droppedDesc == nil {
			continue
		}
		cascadeDroppedViews, err := params.p.dropViewImpl(ctx, droppedDesc, n.n.DropBehavior)
		if err != nil {
			return err
		}
		// Log a Drop View event for this table. This is an auditable log event
		// and is recorded in the same transaction as the table descriptor
		// update.
		if err := MakeEventLogger(params.p.LeaseMgr()).InsertEventRecord(
			ctx,
			params.p.txn,
			EventLogDropView,
			int32(droppedDesc.ID),
			int32(params.evalCtx.NodeID),
			struct {
				ViewName            string
				Statement           string
				User                string
				CascadeDroppedViews []string
			}{droppedDesc.Name, n.n.String(), params.p.session.User, cascadeDroppedViews},
		); err != nil {
			return err
		}
	}
	return nil
}

func (*dropViewNode) Next(runParams) (bool, error) { return false, nil }
func (*dropViewNode) Values() tree.Datums          { return tree.Datums{} }
func (*dropViewNode) Close(context.Context)        {}

func descInSlice(descID sqlbase.ID, td []*sqlbase.TableDescriptor) bool {
	for _, desc := range td {
		if descID == desc.ID {
			return true
		}
	}
	return false
}

func (p *planner) canRemoveDependentView(
	ctx context.Context,
	from *sqlbase.TableDescriptor,
	ref sqlbase.TableDescriptor_Reference,
	behavior tree.DropBehavior,
) error {
	return p.canRemoveDependentViewGeneric(ctx, from.TypeName(), from.Name, from.ParentID, ref, behavior)
}

func (p *planner) canRemoveDependentViewGeneric(
	ctx context.Context,
	typeName string,
	objName string,
	parentID sqlbase.ID,
	ref sqlbase.TableDescriptor_Reference,
	behavior tree.DropBehavior,
) error {
	viewDesc, err := p.getViewDescForCascade(ctx, typeName, objName, parentID, ref.ID, behavior)
	if err != nil {
		return err
	}
	if err := p.CheckPrivilege(viewDesc, privilege.DROP); err != nil {
		return err
	}
	// If this view is depended on by other views, we have to check them as well.
	for _, ref := range viewDesc.DependedOnBy {
		if err := p.canRemoveDependentView(ctx, viewDesc, ref, behavior); err != nil {
			return err
		}
	}
	return nil
}

// Drops the view and any additional views that depend on it.
// Returns the names of any additional views that were also dropped
// due to `cascade` behavior.
func (p *planner) removeDependentView(
	ctx context.Context, tableDesc, viewDesc *sqlbase.TableDescriptor,
) ([]string, error) {
	// In the table whose index is being removed, filter out all back-references
	// that refer to the view that's being removed.
	tableDesc.DependedOnBy = removeMatchingReferences(tableDesc.DependedOnBy, viewDesc.ID)
	// Then proceed to actually drop the view and log an event for it.
	return p.dropViewImpl(ctx, viewDesc, tree.DropCascade)
}

// dropViewImpl does the work of dropping a view (and views that depend on it
// if `cascade is specified`). Returns the names of any additional views that
// were also dropped due to `cascade` behavior.
func (p *planner) dropViewImpl(
	ctx context.Context, viewDesc *sqlbase.TableDescriptor, behavior tree.DropBehavior,
) ([]string, error) {
	var cascadeDroppedViews []string

	// Remove back-references from the tables/views this view depends on.
	for _, depID := range viewDesc.DependsOn {
		dependencyDesc, err := sqlbase.GetTableDescFromID(ctx, p.txn, depID)
		if err != nil {
			return cascadeDroppedViews,
				errors.Errorf("error resolving dependency relation ID %d: %v", depID, err)
		}
		// The dependency is also being deleted, so we don't have to remove the
		// references.
		if dependencyDesc.Dropped() {
			continue
		}
		dependencyDesc.DependedOnBy = removeMatchingReferences(dependencyDesc.DependedOnBy, viewDesc.ID)
		if err := p.saveNonmutationAndNotify(ctx, dependencyDesc); err != nil {
			return cascadeDroppedViews, err
		}
	}
	viewDesc.DependsOn = nil

	if behavior == tree.DropCascade {
		for _, ref := range viewDesc.DependedOnBy {
			dependentDesc, err := p.getViewDescForCascade(
				ctx, viewDesc.TypeName(), viewDesc.Name, viewDesc.ParentID, ref.ID, behavior,
			)
			if err != nil {
				return cascadeDroppedViews, err
			}
			cascadedViews, err := p.dropViewImpl(ctx, dependentDesc, behavior)
			if err != nil {
				return cascadeDroppedViews, err
			}
			cascadeDroppedViews = append(cascadeDroppedViews, cascadedViews...)
			cascadeDroppedViews = append(cascadeDroppedViews, dependentDesc.Name)
		}
	}

	if err := p.initiateDropTable(ctx, viewDesc); err != nil {
		return cascadeDroppedViews, err
	}

	p.session.setTestingVerifyMetadata(func(systemConfig config.SystemConfig) error {
		return verifyDropTableMetadata(systemConfig, viewDesc.ID, "view")
	})
	return cascadeDroppedViews, nil
}

func (p *planner) getViewDescForCascade(
	ctx context.Context,
	typeName string,
	objName string,
	parentID, viewID sqlbase.ID,
	behavior tree.DropBehavior,
) (*sqlbase.TableDescriptor, error) {
	viewDesc, err := sqlbase.GetTableDescFromID(ctx, p.txn, viewID)
	if err != nil {
		log.Warningf(ctx, "unable to retrieve descriptor for view %d: %v", viewID, err)
		return nil, errors.Wrapf(err, "error resolving dependent view ID %d", viewID)
	}
	if behavior != tree.DropCascade {
		viewName := viewDesc.Name
		if viewDesc.ParentID != parentID {
			var err error
			viewName, err = p.getQualifiedTableName(ctx, viewDesc)
			if err != nil {
				log.Warningf(ctx, "unable to retrieve qualified name of view %d: %v", viewID, err)
				msg := fmt.Sprintf("cannot drop %s %q because a view depends on it", typeName, objName)
				return nil, sqlbase.NewDependentObjectError(msg)
			}
		}
		msg := fmt.Sprintf("cannot drop %s %q because view %q depends on it",
			typeName, objName, viewName)
		hint := fmt.Sprintf("you can drop %s instead.", viewName)
		return nil, sqlbase.NewDependentObjectErrorWithHint(msg, hint)
	}
	return viewDesc, nil
}
