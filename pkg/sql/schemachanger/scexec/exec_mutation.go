// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package scexec

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/nstree"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scexec/scmutationexec"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scop"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/util/log/eventpb"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

func executeDescriptorMutationOps(ctx context.Context, deps Dependencies, ops []scop.Op) error {
	mvs := newMutationVisitorState(deps.Catalog())
	v := scmutationexec.NewMutationVisitor(deps.Catalog(), mvs, deps.Partitioner())
	for _, op := range ops {
		if err := op.(scop.MutationOp).Visit(ctx, v); err != nil {
			return err
		}
	}
	b := deps.Catalog().NewCatalogChangeBatcher()
	err := mvs.checkedOutDescriptors.IterateByID(func(entry catalog.NameEntry) error {
		return b.CreateOrUpdateDescriptor(ctx, entry.(catalog.MutableDescriptor))
	})
	if err != nil {
		return err
	}
	for id, drainedNames := range mvs.drainedNames {
		for _, name := range drainedNames {
			if err := b.DeleteName(ctx, name, id); err != nil {
				return err
			}
		}
	}
	// Any databases being GCed should have an entry even if none of its tables
	// are being dropped. This entry will be used to generate the GC jobs below.
	for _, dbID := range mvs.dbGCJobs.Ordered() {
		if _, ok := mvs.descriptorGCJobs[dbID]; !ok {
			mvs.descriptorGCJobs[dbID] = nil
		}
	}
	if len(mvs.descriptorGCJobs) > 0 ||
		mvs.dbGCJobs.Len() > 0 {
		for parentID := range mvs.descriptorGCJobs {
			job := jobspb.SchemaChangeGCDetails{
				Tables: mvs.descriptorGCJobs[parentID],
			}
			// Check if the database is also being cleaned up at the same time.
			if mvs.dbGCJobs.Contains(parentID) {
				job.ParentID = parentID
			}
			jobName := func() string {
				if len(mvs.descriptorGCJobs[parentID]) == 1 &&
					job.ParentID == descpb.InvalidID {
					return fmt.Sprintf("dropping descriptor %d", mvs.descriptorGCJobs[parentID][0].ID)
				}
				var sb strings.Builder
				sb.WriteString("dropping descriptors")
				for _, table := range mvs.descriptorGCJobs[parentID] {
					sb.WriteString(fmt.Sprintf(" %d", table.ID))
				}
				if job.ParentID != descpb.InvalidID {
					sb.WriteString(fmt.Sprintf(" and parent database %d", job.ParentID))
				}
				return sb.String()
			}
			record := createGCJobRecord(jobName(), security.NodeUserName(), job)
			record.JobID = deps.TransactionalJobCreator().MakeJobID()
			if err := deps.TransactionalJobCreator().CreateJob(ctx, record); err != nil {
				return err
			}
		}
	}
	for tableID, indexes := range mvs.indexGCJobs {
		job := jobspb.SchemaChangeGCDetails{
			ParentID: tableID,
			Indexes:  indexes,
		}
		jobName := func() string {
			if len(indexes) == 1 {
				return fmt.Sprintf("dropping table %d index %d", tableID, indexes[0].IndexID)
			}
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("dropping table %d indexes", tableID))
			for _, index := range indexes {
				sb.WriteString(fmt.Sprintf(" %d", index.IndexID))
			}
			return sb.String()
		}

		record := createGCJobRecord(jobName(), security.NodeUserName(), job)
		record.JobID = deps.TransactionalJobCreator().MakeJobID()
		if err := deps.TransactionalJobCreator().CreateJob(ctx, record); err != nil {
			return err
		}
	}
	if mvs.schemaChangerJob != nil {
		if err := deps.TransactionalJobCreator().CreateJob(ctx, *mvs.schemaChangerJob); err != nil {
			return err
		}
	}

	statementIDs := make([]uint32, 0, len(mvs.eventsByStatement))
	for statementID := range mvs.eventsByStatement {
		statementIDs = append(statementIDs, statementID)
	}
	sort.Slice(statementIDs, func(i, j int) bool {
		return statementIDs[i] < statementIDs[j]
	})
	for _, statementID := range statementIDs {
		entries := eventLogEntriesForStatement(mvs.eventsByStatement[statementID])
		for _, e := range entries {
			// TODO(postamar): batch these
			if err := deps.EventLogger().LogEvent(ctx, e.id, *e.metadata, e.event); err != nil {
				return err
			}
		}
	}

	for _, id := range mvs.descriptorsToDelete.Ordered() {
		if err := b.DeleteDescriptor(ctx, id); err != nil {
			return err
		}
	}
	for id, update := range mvs.schemaChangerStatusUpdates {
		if err := deps.TransactionalJobCreator().UpdateSchemaChangeJob(ctx, id, func(
			md jobs.JobMetadata, updateProgress func(*jobspb.Progress),
		) error {
			progress := *md.Progress
			progress.GetNewSchemaChange().States = update
			updateProgress(&progress)
			return nil
		}); err != nil {
			return err
		}
	}
	return b.ValidateAndRun(ctx)
}

func eventLogEntriesForStatement(statementEvents []eventPayload) (logEntries []eventPayload) {
	// A dependent event is one which is generated because of a
	// dependency getting modified from the source object. An example
	// of this is a DROP TABLE will be the source event, which will track
	// any dependent views dropped.
	var dependentEvents = make(map[uint32][]eventPayload)
	var sourceEvents = make(map[uint32]eventPayload)
	// First separate out events, where the first event generated will always
	// be the source and everything else before will be dependencies if they have
	// the same subtask ID.
	for _, event := range statementEvents {
		dependentEvents[event.metadata.SubWorkID] = append(dependentEvents[event.metadata.SubWorkID], event)
	}
	// Split of the source events.
	orderedSubWorkID := make([]uint32, 0, len(dependentEvents))
	for subWorkID := range dependentEvents {
		elems := dependentEvents[subWorkID]
		sort.SliceStable(elems, func(i, j int) bool {
			return elems[i].metadata.SourceElementID < elems[j].metadata.SourceElementID
		})
		sourceEvents[subWorkID] = elems[0]
		dependentEvents[subWorkID] = elems[1:]
		orderedSubWorkID = append(orderedSubWorkID, subWorkID)
	}
	// Store an ordered list of sub-work IDs for deterministic
	// event order.
	sort.SliceStable(orderedSubWorkID, func(i, j int) bool {
		return orderedSubWorkID[i] < orderedSubWorkID[j]
	})
	// Collect the dependent objects for each
	// source event, and generate an event log entry.
	for _, subWorkID := range orderedSubWorkID {
		// Determine which objects we should collect.
		collectDependentViewNames := false
		collectDependentSchemaNames := false
		sourceEvent := sourceEvents[subWorkID]
		switch sourceEvent.event.(type) {
		case *eventpb.DropDatabase:
			// Drop database only reports dependent schemas.
			collectDependentSchemaNames = true
		case *eventpb.DropView, *eventpb.DropTable:
			// Drop view and drop tables only cares about
			// dependent views
			collectDependentViewNames = true
		}
		var dependentObjects []string
		for _, dependentEvent := range dependentEvents[subWorkID] {
			switch ev := dependentEvent.event.(type) {
			case *eventpb.DropView:
				if collectDependentViewNames {
					dependentObjects = append(dependentObjects, ev.ViewName)
				}
			case *eventpb.DropSchema:
				if collectDependentSchemaNames {
					dependentObjects = append(dependentObjects, ev.SchemaName)
				}
			}
		}
		// Add anything that we determined based
		// on the dependencies.
		switch ev := sourceEvent.event.(type) {
		case *eventpb.DropTable:
			ev.CascadeDroppedViews = dependentObjects
		case *eventpb.DropView:
			ev.CascadeDroppedViews = dependentObjects
		case *eventpb.DropDatabase:
			ev.DroppedSchemaObjects = dependentObjects
		}
		// Generate event log entries for the source event only. The dependent
		// events will be ignored.
		logEntries = append(logEntries, sourceEvent)
	}
	return logEntries
}

type mutationVisitorState struct {
	c                          Catalog
	checkedOutDescriptors      nstree.Map
	drainedNames               map[descpb.ID][]descpb.NameInfo
	descriptorsToDelete        catalog.DescriptorIDSet
	dbGCJobs                   catalog.DescriptorIDSet
	descriptorGCJobs           map[descpb.ID][]jobspb.SchemaChangeGCDetails_DroppedID
	indexGCJobs                map[descpb.ID][]jobspb.SchemaChangeGCDetails_DroppedIndex
	schemaChangerJob           *jobs.Record
	schemaChangerStatusUpdates map[jobspb.JobID][]scpb.Status
	eventsByStatement          map[uint32][]eventPayload
}

type eventPayload struct {
	id       descpb.ID
	metadata *scpb.ElementMetadata
	event    eventpb.EventPayload
}

func (mvs *mutationVisitorState) UpdateSchemaChangerJobProgress(
	jobID jobspb.JobID, statuses []scpb.Status,
) error {
	if mvs.schemaChangerStatusUpdates == nil {
		mvs.schemaChangerStatusUpdates = make(map[jobspb.JobID][]scpb.Status)
	} else if _, exists := mvs.schemaChangerStatusUpdates[jobID]; exists {
		return errors.AssertionFailedf("cannot update job %d more than once", jobID)
	}
	mvs.schemaChangerStatusUpdates[jobID] = statuses
	return nil
}

func newMutationVisitorState(c Catalog) *mutationVisitorState {
	return &mutationVisitorState{
		c:                 c,
		drainedNames:      make(map[descpb.ID][]descpb.NameInfo),
		indexGCJobs:       make(map[descpb.ID][]jobspb.SchemaChangeGCDetails_DroppedIndex),
		descriptorGCJobs:  make(map[descpb.ID][]jobspb.SchemaChangeGCDetails_DroppedID),
		eventsByStatement: make(map[uint32][]eventPayload),
	}
}

var _ scmutationexec.MutationVisitorStateUpdater = (*mutationVisitorState)(nil)

func (mvs *mutationVisitorState) CheckOutDescriptor(
	ctx context.Context, id descpb.ID,
) (catalog.MutableDescriptor, error) {
	entry := mvs.checkedOutDescriptors.GetByID(id)
	if entry != nil {
		return entry.(catalog.MutableDescriptor), nil
	}
	mut, err := mvs.c.MustReadMutableDescriptor(ctx, id)
	if err != nil {
		return nil, err
	}
	mut.MaybeIncrementVersion()
	mvs.checkedOutDescriptors.Upsert(mut)
	return mut, nil
}

func (mvs *mutationVisitorState) DeleteDescriptor(id descpb.ID) {
	mvs.descriptorsToDelete.Add(id)
}

func (mvs *mutationVisitorState) AddDrainedName(id descpb.ID, nameInfo descpb.NameInfo) {
	if _, ok := mvs.drainedNames[id]; !ok {
		mvs.drainedNames[id] = []descpb.NameInfo{nameInfo}
	} else {
		mvs.drainedNames[id] = append(mvs.drainedNames[id], nameInfo)
	}
}

func (mvs *mutationVisitorState) AddNewGCJobForTable(table catalog.TableDescriptor) {
	mvs.descriptorGCJobs[table.GetParentID()] = append(mvs.descriptorGCJobs[table.GetParentID()],
		jobspb.SchemaChangeGCDetails_DroppedID{
			ID:       table.GetID(),
			DropTime: timeutil.Now().UnixNano(),
		})
}

func (mvs *mutationVisitorState) AddNewGCJobForDatabase(db catalog.DatabaseDescriptor) {
	mvs.dbGCJobs.Add(db.GetID())
}

func (mvs *mutationVisitorState) AddNewGCJobForIndex(
	tbl catalog.TableDescriptor, index catalog.Index,
) {
	mvs.indexGCJobs[tbl.GetID()] = append(
		mvs.indexGCJobs[tbl.GetID()],
		jobspb.SchemaChangeGCDetails_DroppedIndex{
			IndexID:  index.GetID(),
			DropTime: timeutil.Now().UnixNano(),
		})
}

func (mvs *mutationVisitorState) AddNewSchemaChangerJob(rec jobs.Record) error {
	if mvs.schemaChangerJob != nil {
		return errors.AssertionFailedf("cannot create more than one new schema change job")
	}
	mvs.schemaChangerJob = &rec
	return nil
}

// createGCJobRecord creates the job record for a GC job, setting some
// properties which are common for all GC jobs.
func createGCJobRecord(
	originalDescription string, username security.SQLUsername, details jobspb.SchemaChangeGCDetails,
) jobs.Record {
	descriptorIDs := make([]descpb.ID, 0)
	if len(details.Indexes) > 0 {
		if len(descriptorIDs) == 0 {
			descriptorIDs = []descpb.ID{details.ParentID}
		}
	} else {
		for _, table := range details.Tables {
			descriptorIDs = append(descriptorIDs, table.ID)
		}
	}
	return jobs.Record{
		Description:   fmt.Sprintf("GC for %s", originalDescription),
		Username:      username,
		DescriptorIDs: descriptorIDs,
		Details:       details,
		Progress:      jobspb.SchemaChangeGCProgress{},
		RunningStatus: "waiting for GC TTL",
		NonCancelable: true,
	}
}

// EnqueueEvent implements the scmutationexec.MutationVisitorStateUpdater
// interface.
func (mvs *mutationVisitorState) EnqueueEvent(
	id descpb.ID, metadata *scpb.ElementMetadata, event eventpb.EventPayload,
) error {
	mvs.eventsByStatement[metadata.StatementID] = append(
		mvs.eventsByStatement[metadata.StatementID],
		eventPayload{
			id:       id,
			event:    event,
			metadata: metadata,
		},
	)
	return nil
}
