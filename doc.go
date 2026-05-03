// Package locationindex provides an in-memory spatial index for location codes.
//
// Write and update model:
//
// A LocationIndex is a mutable in-memory structure. Insert, Remove, and Update
// eagerly maintain all derived postings and caches inside the same instance.
// The package does not implement transactions, WAL replay, or multi-version
// storage; callers should treat it as an indexing component rather than a full
// database.
//
// Persistence model:
//
// Save writes a complete point-in-time snapshot of the current index state.
// Open and Load restore a previously saved snapshot. Persisted files are
// snapshot artifacts, not incremental logs.
//
// Recommended service workflow:
//
// A service can Open a persisted index for queries, use Snapshot or Clone when
// it needs staged mutation, and Save a replacement snapshot when it wants to
// publish durable updates. This keeps lifecycle boundaries explicit and avoids
// depending on internal derived structures.
package locationindex
