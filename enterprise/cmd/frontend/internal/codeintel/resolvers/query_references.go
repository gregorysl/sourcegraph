package resolvers

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hashicorp/go-multierror"
	"github.com/opentracing/opentracing-go/log"

	"github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/stores/dbstore"
	store "github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/stores/dbstore"
	"github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/stores/lsifstore"
	"github.com/sourcegraph/sourcegraph/internal/observation"
	"github.com/sourcegraph/sourcegraph/lib/codeintel/bloomfilter"
	"github.com/sourcegraph/sourcegraph/lib/codeintel/precise"
)

const slowReferencesRequestThreshold = time.Second

// References returns the list of source locations that reference the symbol at the given position.
func (r *queryResolver) References(ctx context.Context, line, character, limit int, rawCursor string) (_ []AdjustedLocation, _ string, err error) {
	ctx, traceLog, endObservation := observeResolver(ctx, &err, "References", r.operations.references, slowReferencesRequestThreshold, observation.Args{
		LogFields: []log.Field{
			log.Int("repositoryID", r.repositoryID),
			log.String("commit", r.commit),
			log.String("path", r.path),
			log.Int("numUploads", len(r.uploads)),
			log.String("uploads", uploadIDsToString(r.uploads)),
			log.Int("line", line),
			log.Int("character", character),
		},
	})
	defer endObservation()

	// Decode cursor given from previous response or create a new one with default values.
	// We use the cursor state track offsets with the result set and cache initial data that
	// is used to resolve each page. This cursor will be modified in-place to become the
	// cursor used to fetch the subsequent page of results in this result set.
	cursor, err := decodeCursor(rawCursor)
	if err != nil {
		return nil, "", errors.Wrap(err, fmt.Sprintf("invalid cursor: %q", rawCursor))
	}

	// Adjust the path and position for each visible upload based on its git difference to
	// the target commit. This data may already be stashed in the cursor decoded above, in
	// which case we don't need to hit the database.

	// References at the given file:line:character could come from multiple uploads, so we
	// need to look in all uploads and merge the results.

	adjustedUploads, err := r.adjustedUploadsFromCursor(ctx, line, character, &cursor.AdjustedUploads)
	if err != nil {
		return nil, "", err
	}

	// Gather all monikers attached to the ranges enclosing the requested position. This data
	// may already be stashed in the cursor decoded above, in which case we don't need to hit
	// the database.

	if cursor.OrderedMonikers == nil {
		if cursor.OrderedMonikers, err = r.orderedMonikers(ctx, adjustedUploads, "export"); err != nil {
			return nil, "", err
		}
	}
	traceLog(
		log.Int("numMonikers", len(cursor.OrderedMonikers)),
		log.String("monikers", monikersToString(cursor.OrderedMonikers)),
	)

	// Determine the set of uploads that define one of the ordered monikers. This may include
	// one of the adjusted indexes. This data may already be stashed in the cursor decoded above,
	// in which case we don't need to hit the database.

	definitionUploadIDs, err := r.definitionUploadIDsFromCursor(ctx, adjustedUploads, cursor.OrderedMonikers, &cursor)
	if err != nil {
		return nil, "", err
	}
	traceLog(
		log.Int("numDefinitionUploads", len(definitionUploadIDs)),
		log.String("definitionUploads", intsToString(definitionUploadIDs)),
	)

	// Phase 1: Gather all "local" locations via LSIF graph traversal. We'll continue to request additional
	// locations until we fill an entire page (the size of which is denoted by the given limit) or there are
	// no more local results remaining.
	var locations []lsifstore.Location
	if cursor.Phase == "local" {
		localLocations, hasMore, err := r.pageLocalReferences(ctx, "references", adjustedUploads, &cursor, limit-len(locations))
		if err != nil {
			return nil, "", err
		}
		locations = append(locations, localLocations...)

		if !hasMore {
			// No more local results, move on to phase 2
			cursor.Phase = "remote"
		}
	}

	// Phase 2: Gather all "remote" locations via moniker search. We only do this if there are no more local
	// results. We'll continue to request additional locations until we fill an entire page or there are no
	// more local results remaining, just as we did above.
	if cursor.Phase == "remote" {
		for len(locations) < limit {
			remoteLocations, hasMore, err := r.pageRemoteReferences(ctx, "references", adjustedUploads, cursor.OrderedMonikers, definitionUploadIDs, &cursor, limit-len(locations))
			if err != nil {
				return nil, "", err
			}
			locations = append(locations, remoteLocations...)

			if !hasMore {
				cursor.Phase = "done"
				break
			}
		}
	}

	traceLog(log.Int("numLocations", len(locations)))

	// Adjust the locations back to the appropriate range in the target commits. This adjusts
	// locations within the repository the user is browsing so that it appears all references
	// are occurring at the same commit they are looking at.

	adjustedLocations, err := r.adjustLocations(ctx, locations)
	if err != nil {
		return nil, "", err
	}
	traceLog(log.Int("numAdjustedLocations", len(adjustedLocations)))

	nextCursor := ""
	if cursor.Phase != "done" {
		nextCursor = encodeCursor(cursor)
	}

	return adjustedLocations, nextCursor, nil
}

// ErrConcurrentModification occurs when a page of a references request cannot be resolved as
// the set of visible uploads have changed since the previous request for the same result set.
var ErrConcurrentModification = errors.New("result set changed while paginating")

// adjustedUploadsFromCursor adjusts the current target path and the given position for each upload
// visible from the current target commit. If an upload cannot be adjusted, it will be omitted from
// the returned slice. The returned slice will be cached on the given cursor. If this data is already
// stashed on the given cursor, the result is recalculated from the cursor data/resolver context, and
// we don't need to hit the database.
//
// An error is returned if the set of visible uploads has changed since the previous request of this
// result set (specifically if an index becomes invisible). This behavior may change in the future.
func (r *queryResolver) adjustedUploadsFromCursor(ctx context.Context, line, character int, cursorAdjustedUploads *[]cursorAdjustedUpload) ([]adjustedUpload, error) {
	if *cursorAdjustedUploads != nil {
		adjustedUploads := make([]adjustedUpload, 0, len(*cursorAdjustedUploads))
		for _, u := range *cursorAdjustedUploads {
			upload, ok := r.uploadCache[u.DumpID]
			if !ok {
				return nil, ErrConcurrentModification
			}

			adjustedUploads = append(adjustedUploads, adjustedUpload{
				Upload:               upload,
				AdjustedPath:         u.AdjustedPath,
				AdjustedPosition:     u.AdjustedPosition,
				AdjustedPathInBundle: u.AdjustedPathInBundle,
			})
		}

		return adjustedUploads, nil
	}

	adjustedUploads, err := r.adjustUploads(ctx, line, character)
	if err != nil {
		return nil, err
	}

	*cursorAdjustedUploads = make([]cursorAdjustedUpload, 0, len(adjustedUploads))
	for i := range adjustedUploads {
		*cursorAdjustedUploads = append(*cursorAdjustedUploads, cursorAdjustedUpload{
			DumpID:               adjustedUploads[i].Upload.ID,
			AdjustedPath:         adjustedUploads[i].AdjustedPath,
			AdjustedPosition:     adjustedUploads[i].AdjustedPosition,
			AdjustedPathInBundle: adjustedUploads[i].AdjustedPathInBundle,
		})
	}

	return adjustedUploads, nil
}

// definitionUploadIDsFromCursor returns a set of identifiers for uploads that provide any of the given
// monikers. Uploads already present in the given adjusted uploads slice will be omitted from the returned
// slice. The returned slice will be cached on the given cursor. If this data is already stashed in the
// given cursor, we don't need to hit the database.
//
// The upload records returned from the database, if any, are also returned from this method to help reduce
// the number of database queries necessary.
func (r *queryResolver) definitionUploadIDsFromCursor(ctx context.Context, adjustedUploads []adjustedUpload, orderedMonikers []precise.QualifiedMonikerData, cursor *referencesCursor) ([]int, error) {
	if cursor.DefinitionUploadIDs != nil {
		return cursor.DefinitionUploadIDs, nil
	}

	definitionUploads, err := r.definitionUploads(ctx, orderedMonikers)
	if err != nil {
		return nil, err
	}

	// Omit the given adjusted uploads
	definitionUploadIDs := make([]int, 0, len(definitionUploads))
	for i := range definitionUploads {
		found := false
		for j := range adjustedUploads {
			if definitionUploads[i].ID == adjustedUploads[j].Upload.ID {
				found = true
				break
			}
		}
		if !found {
			definitionUploadIDs = append(definitionUploadIDs, definitionUploads[i].ID)
		}
	}

	// Stash the definition upload IDs and set a flag indicating their presence. We set a flag explicitly
	// to avoid ambiguity between no data in the cursor and an empty list in the cursor.

	cursor.DefinitionUploadIDs = definitionUploadIDs
	return definitionUploadIDs, nil
}

// pageLocalReferences returns a slice of the (local) result set denoted by the given cursor fulfilled by
// traversing the LSIF graph. The given cursor will be adjusted to reflect the offsets required to resolve
// the next page of results. If there are no more pages left in the result set, a false-valued flag is
// returned.
func (r *queryResolver) pageLocalReferences(ctx context.Context, ty string, adjustedUploads []adjustedUpload, cursor *referencesCursor, limit int) ([]lsifstore.Location, bool, error) {
	var allLocations []lsifstore.Location
	for _, adjustedUpload := range adjustedUploads[cursor.LocalBatchOffset:] {
		if len(allLocations) >= limit {
			// We've filled the page
			break
		}

		fn := r.lsifStore.References
		if ty == "implementations" {
			fn = r.lsifStore.Implementations
		}
		locations, totalCount, err := fn(
			ctx,
			adjustedUpload.Upload.ID,
			adjustedUpload.AdjustedPathInBundle,
			adjustedUpload.AdjustedPosition.Line,
			adjustedUpload.AdjustedPosition.Character,
			limit-len(allLocations),
			cursor.LocalOffset,
		)
		if err != nil {
			return nil, false, errors.Wrap(err, "lsifstore.References")
		}

		cursor.LocalOffset += len(locations)

		if cursor.LocalOffset >= totalCount {
			// Skip this index on next request
			cursor.LocalOffset = 0
			cursor.LocalBatchOffset++
		}

		allLocations = append(allLocations, locations...)
	}

	return allLocations, cursor.LocalBatchOffset < len(adjustedUploads), nil
}

// maximumIndexesPerMonikerSearch configures the maximum number of reference upload identifiers
// that can be passed to a single moniker search query.
const maximumIndexesPerMonikerSearch = 50

// pageRemoteReferences returns a slice of the (remote) result set denoted by the given cursor fulfilled by
// performing a moniker search over a group of indexes. The given cursor will be adjusted to reflect the
// offsets required to resolve the next page of results. If there are no more pages left in the result set,
// a false-valued flag is returned.
func (r *queryResolver) pageRemoteReferences(ctx context.Context, lsifDataTable string, adjustedUploads []adjustedUpload, orderedMonikers []precise.QualifiedMonikerData, definitionUploadIDs []int, cursor *referencesCursor, limit int) ([]lsifstore.Location, bool, error) {
	for len(cursor.BatchIDs) == 0 {
		if cursor.RemoteBatchOffset < 0 {
			// No more batches
			return nil, false, nil
		}

		// Find the next batch of indexes to perform a moniker search over
		referenceUploadIDs, recordScanned, totalCount, err := r.uploadIDsWithReferences(ctx, orderedMonikers, definitionUploadIDs, maximumIndexesPerMonikerSearch, cursor.RemoteBatchOffset)
		if err != nil {
			return nil, false, err
		}

		cursor.BatchIDs = referenceUploadIDs
		cursor.RemoteBatchOffset += recordScanned

		if cursor.RemoteBatchOffset >= totalCount {
			// Signal no batches remaining
			cursor.RemoteBatchOffset = -1
		}
	}

	// Fetch the upload records we don't currently have hydrated and insert them into the map
	monikerSearchUploads, err := r.uploadsByIDs(ctx, cursor.BatchIDs)
	if err != nil {
		return nil, false, err
	}

	// Perform the moniker search
	locations, totalCount, err := r.monikerLocations(ctx, monikerSearchUploads, orderedMonikers, lsifDataTable, limit, cursor.RemoteOffset)
	if err != nil {
		return nil, false, err
	}

	cursor.RemoteOffset += len(locations)

	if cursor.RemoteOffset >= totalCount {
		// Require a new batch on next page
		cursor.RemoteOffset = 0
		cursor.BatchIDs = nil
	}

	// Perform an in-place filter to remove specific duplicate locations. Ranges that enclose the
	// target position will be returned by both an LSIF graph traversal as well as a moniker search.
	// We remove the latter instances.

	filtered := locations[:0]

	for _, location := range locations {
		if !isSourceLocation(adjustedUploads, location) {
			filtered = append(filtered, location)
		}
	}

	// We have another page if we still have results in the current batch of reference indexes, or if
	// we can query a next batch of reference indexes. We may return true here when we are actually
	// out of references. This behavior may change in the future.
	return filtered, len(cursor.BatchIDs) > 0 || cursor.RemoteBatchOffset >= 0, nil
}

// isSourceLocation returns true if the given location encloses the source position within one of the visible uploads.
func isSourceLocation(adjustedUploads []adjustedUpload, location lsifstore.Location) bool {
	for i := range adjustedUploads {
		if location.DumpID == adjustedUploads[i].Upload.ID && location.Path == adjustedUploads[i].AdjustedPath {
			if rangeContainsPosition(location.Range, adjustedUploads[i].AdjustedPosition) {
				return true
			}
		}
	}

	return false
}

// rangeContainsPosition returns true if the given range encloses the given position.
func rangeContainsPosition(r lsifstore.Range, pos lsifstore.Position) bool {
	if pos.Line < r.Start.Line {
		return false
	}

	if pos.Line > r.End.Line {
		return false
	}

	if pos.Line == r.Start.Line && pos.Character < r.Start.Character {
		return false
	}

	if pos.Line == r.End.Line && pos.Character > r.End.Character {
		return false
	}

	return true
}

// uploadIDsWithReferences returns uploads (ignoring the given uploads) that probably contain an import
// or implementation moniker whose identifier matches any of the given monikers' identifiers. This method
// will not return uploads for commits which are unknown to gitserver, nor will it return uploads which
// are listed in the given ignored identifier slice. This method also returns the number of records
// scanned (but possibly filtered out from the return slice) from the database (the offset for the
// subsequent request) and the total number of records in the database.
func (r *queryResolver) uploadIDsWithReferences(ctx context.Context, orderedMonikers []precise.QualifiedMonikerData, ignoreIDs []int, limit, offset int) (ids []int, recordsScanned int, totalCount int, err error) {
	scanner, totalCount, err := r.dbStore.ReferenceIDsAndFilters(ctx, r.repositoryID, r.commit, orderedMonikers, limit, offset)
	if err != nil {
		return nil, 0, 0, errors.Wrap(err, "dbstore.ReferenceIDsAndFilters")
	}

	defer func() {
		if closeErr := scanner.Close(); closeErr != nil {
			err = multierror.Append(err, errors.Wrap(closeErr, "dbstore.ReferenceIDsAndFilters.Close"))
		}
	}()

	ignoreIDsMap := map[int]struct{}{}
	for id := range ignoreIDs {
		ignoreIDsMap[id] = struct{}{}
	}

	filtered := map[int]struct{}{}

	for len(filtered) < limit {
		packageReference, exists, err := scanner.Next()
		if err != nil {
			return nil, 0, 0, errors.Wrap(err, "dbstore.ReferenceIDsAndFilters.Next")
		}
		if !exists {
			break
		}
		recordsScanned++

		if _, ok := filtered[packageReference.DumpID]; ok {
			// This index includes a definition so we can skip testing the filters here. The index
			// will be included in the moniker search regardless if it contains additional references.
			continue
		}

		if _, ok := ignoreIDsMap[packageReference.DumpID]; ok {
			// Already in set, don't duplicate tests
			continue
		}

		// Each upload has an associated bloom filter encoding the set of identifiers it imports or
		// implements. We test this bloom filter to greatly reduce the number of remote indexes over
		// which we need to search.

		ok, err := testFilter(packageReference.Filter, orderedMonikers)
		if err != nil {
			return nil, 0, 0, err
		}
		if ok {
			// Probably imports or implements at least one of the monikers' identifiers
			filtered[packageReference.DumpID] = struct{}{}
		}
	}

	flattened := make([]int, 0, len(filtered))
	for k := range filtered {
		flattened = append(flattened, k)
	}
	sort.Ints(flattened)

	return flattened, recordsScanned, totalCount, nil
}

// testFilter returns true if the set underlying the given encoded bloom filter probably includes any of
// the given monikers.
func testFilter(filter []byte, orderedMonikers []precise.QualifiedMonikerData) (bool, error) {
	includesIdentifier, err := bloomfilter.Decode(filter)
	if err != nil {
		return false, errors.Wrap(err, "bloomfilter.Decode")
	}

	for _, moniker := range orderedMonikers {
		if includesIdentifier(moniker.Identifier) {
			return true, nil
		}
	}

	return false, nil
}

// uploadsByIDs returns a slice of uploads with the given identifiers. This method will not return a
// new upload record for a commit which is unknown to gitserver. The given upload map is used as a
// caching mechanism - uploads present in the map are not fetched again from the database.
func (r *queryResolver) uploadsByIDs(ctx context.Context, ids []int) ([]dbstore.Dump, error) {
	missingIDs := make([]int, 0, len(ids))
	existingUploads := make([]store.Dump, 0, len(ids))

	for _, id := range ids {
		if upload, ok := r.uploadCache[id]; ok {
			existingUploads = append(existingUploads, upload)
		} else {
			missingIDs = append(missingIDs, id)
		}
	}

	uploads, err := r.dbStore.GetDumpsByIDs(ctx, missingIDs)
	if err != nil {
		return nil, errors.Wrap(err, "dbstore.GetDumpsByIDs")
	}

	newUploads, err := filterUploadsWithCommits(ctx, r.cachedCommitChecker, uploads)
	if err != nil {
		return nil, nil
	}

	for i := range newUploads {
		r.uploadCache[newUploads[i].ID] = newUploads[i]
	}

	return append(existingUploads, newUploads...), nil
}
