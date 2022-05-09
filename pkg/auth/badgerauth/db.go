// Copyright (C) 2022 Storj Labs, Inc.
// See LICENSE for copying information.

package badgerauth

import (
	"bytes"
	"context"
	"time"

	badger "github.com/outcaste-io/badger/v3"
	"github.com/spacemonkeygo/monkit/v3"
	"github.com/zeebo/errs"
	"go.uber.org/zap"

	"storj.io/gateway-mt/pkg/auth/authdb"
	"storj.io/gateway-mt/pkg/auth/badgerauth/pb"
)

const (
	recordTerminatedEventName = "record_terminated"
	nodeIDKey                 = "node_id"
)

var (
	// Below is a compile-time check ensuring DB implements the KV interface.
	_ authdb.KV = (*DB)(nil)

	// ProtoError is a class of proto errors.
	ProtoError = errs.Class("proto")

	// ErrKeyAlreadyExists is an error returned when putting a key that exists.
	ErrKeyAlreadyExists = Error.New("key already exists")

	// ErrDBStartedWithDifferentNodeID is returned when a database is started with a different node id.
	ErrDBStartedWithDifferentNodeID = errs.Class("wrong node id")

	errOperationNotSupported           = Error.New("operation not supported")
	errKeyAlreadyExistsRecordsNotEqual = Error.New("key already exists and records aren't equal")
)

type action int

const (
	put action = iota
	get
)

func (a action) String() string {
	switch a {
	case put:
		return "put"
	case get:
		return "get"
	default:
		return "unknown"
	}
}

// DB represents authentication storage based on BadgerDB.
// This implements the data-storage layer for a distributed Node.
type DB struct {
	db *badger.DB

	config Config
}

// OpenDB opens the underlying database for badgerauth node.
func OpenDB(log *zap.Logger, config Config) (*DB, error) {
	opt := badger.DefaultOptions(config.Path).WithInMemory(config.Path == "")
	if log != nil {
		opt = opt.WithLogger(badgerLogger{log.Sugar().Named("storage")})
	}

	db := &DB{
		config: config,
	}

	var err error
	db.db, err = badger.Open(opt)
	if err != nil {
		return nil, Error.New("open: %w", err)
	}
	if err := db.prepare(); err != nil {
		_ = db.db.Close()
		return nil, Error.New("prepare: %w", err)
	}
	return db, nil
}

// prepare ensures there's a value in the database.
// this allows to ensure that the database is functional.
func (db *DB) prepare() (err error) {
	defer mon.Task(db.eventTags(put)...)(nil)(&err)
	return db.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(nodeIDKey))
		if err != nil {
			if errs.Is(err, badger.ErrKeyNotFound) {
				return Error.Wrap(txn.Set([]byte(nodeIDKey), db.config.ID.Bytes()))
			}
		}

		return Error.Wrap(item.Value(func(val []byte) error {
			if !bytes.Equal(val, db.config.ID.Bytes()) {
				return ErrDBStartedWithDifferentNodeID.New("database %x, configuration %x", val, db.config.ID.Bytes())
			}
			return nil
		}))
	})
}

// Close closes the underlying BadgerDB database.
func (db *DB) Close() error {
	return Error.Wrap(db.db.Close())
}

// Put is like PutAtTime, but it uses current time to store the record.
func (db *DB) Put(ctx context.Context, keyHash authdb.KeyHash, record *authdb.Record) error {
	return db.PutAtTime(ctx, keyHash, record, time.Now())
}

// PutAtTime stores the record at a specific time.
// It is an error if the key already exists.
func (db *DB) PutAtTime(ctx context.Context, keyHash authdb.KeyHash, record *authdb.Record, now time.Time) (err error) {
	defer mon.Task(db.eventTags(put)...)(&ctx)(&err)

	// The check below is to make sure we conform to the KV interface
	// definition, and it's performed outside of the transaction because it's
	// not crucial (access key hashes are unique enough).
	if err = db.db.View(func(txn *badger.Txn) error {
		if _, err = txn.Get(keyHash[:]); err == nil {
			return ErrKeyAlreadyExists
		} else if !errs.Is(err, badger.ErrKeyNotFound) {
			return err
		}
		return nil
	}); err != nil {
		return Error.Wrap(err)
	}

	r := pb.Record{
		CreatedAtUnix:        now.Unix(),
		Public:               record.Public,
		SatelliteAddress:     record.SatelliteAddress,
		MacaroonHead:         record.MacaroonHead,
		ExpiresAtUnix:        timeToTimestamp(record.ExpiresAt),
		EncryptedSecretKey:   record.EncryptedSecretKey,
		EncryptedAccessGrant: record.EncryptedAccessGrant,
		State:                pb.Record_CREATED,
	}

	return Error.Wrap(db.txnWithBackoff(ctx, func(txn *badger.Txn) error {
		return InsertRecord(txn, db.config.ID, keyHash, &r)
	}))
}

// Get retrieves the record from the key/value store. It returns nil if the key
// does not exist. If the record is invalid, the error contains why.
func (db *DB) Get(ctx context.Context, keyHash authdb.KeyHash) (record *authdb.Record, err error) {
	defer mon.Task(db.eventTags(get)...)(&ctx)(&err)

	return record, Error.Wrap(db.db.View(func(txn *badger.Txn) error {
		r, err := lookupRecord(txn, keyHash)
		if err != nil {
			if errs.Is(err, badger.ErrKeyNotFound) {
				return nil
			}
			return err
		}

		if r.InvalidationReason != "" {
			db.monitorEvent(recordTerminatedEventName, get)
			return authdb.Invalid.New("%s", r.InvalidationReason)
		}

		record = &authdb.Record{
			SatelliteAddress:     r.SatelliteAddress,
			MacaroonHead:         r.MacaroonHead,
			EncryptedSecretKey:   r.EncryptedSecretKey,
			EncryptedAccessGrant: r.EncryptedAccessGrant,
			ExpiresAt:            timestampToTime(r.ExpiresAtUnix),
			Public:               r.Public,
		}

		return nil
	}))
}

// DeleteUnused always returns an error because expiring records are deleted by
// default.
func (db *DB) DeleteUnused(context.Context, time.Duration, int, int) (int64, int64, map[string]int64, error) {
	return 0, 0, nil, Error.New("expiring records are deleted by default")
}

// Ping attempts to do a database roundtrip and returns an error if it can't.
func (db *DB) Ping(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)

	err = db.db.View(func(txn *badger.Txn) error {
		_, err := txn.Get([]byte(nodeIDKey))
		return err
	})
	if err != nil {
		return Error.New("unable to read start time: %w", err)
	}

	return nil
}

// UnderlyingDB returns underlying BadgerDB. This method is most useful in
// tests.
func (db *DB) UnderlyingDB() *badger.DB {
	return db.db
}

func (db *DB) txnWithBackoff(ctx context.Context, f func(txn *badger.Txn) error) error {
	// db.config.ConflictBackoff needs to be copied. Otherwise, we are using one
	// for all queries.
	conflictBackoff := db.config.ConflictBackoff
	for {
		if err := db.db.Update(f); err != nil {
			if errs.Is(err, badger.ErrConflict) && !conflictBackoff.Maxed() {
				if err := conflictBackoff.Wait(ctx); err != nil {
					return err
				}
				continue
			}
			return err
		}
		return nil
	}
}

// findResponseEntries finds replication log entries later than a supplied clock
// for a supplied nodeID and matches them with corresponding records to output
// replication response entries.
func (db *DB) findResponseEntries(nodeID NodeID, clock Clock) ([]*pb.ReplicationResponseEntry, error) {
	var response []*pb.ReplicationResponseEntry

	return response, db.db.View(func(txn *badger.Txn) error {
		opt := badger.DefaultIteratorOptions
		opt.PrefetchValues = false
		opt.Prefix = append([]byte(replicationLogPrefix), nodeID.Bytes()...)

		startKey := makeIterationStartKey(nodeID, clock+1)

		it := txn.NewIterator(opt)
		defer it.Close()

		var count int
		for it.Seek(startKey); it.Valid(); it.Next() {
			if count == db.config.ReplicationLimit {
				break
			}
			var entry ReplicationLogEntry
			if err := entry.SetBytes(it.Item().Key()); err != nil {
				return err
			}
			r, err := lookupRecord(txn, entry.KeyHash)
			if err != nil {
				return err
			}
			response = append(response, &pb.ReplicationResponseEntry{
				NodeId:            entry.ID.Bytes(),
				EncryptionKeyHash: entry.KeyHash[:],
				Record:            r,
			})
			count++
		}

		return nil
	})
}

// ensureClock ensures an initial clock (=0) exists for a given id.
func (db *DB) ensureClock(ctx context.Context, id NodeID) (err error) {
	defer mon.Task()(&ctx)(&err)

	return Error.Wrap(db.txnWithBackoff(ctx, func(txn *badger.Txn) error {
		return ensureClock(txn, id)
	}))
}

func (db *DB) buildRequestEntries() ([]*pb.ReplicationRequestEntry, error) {
	var request []*pb.ReplicationRequestEntry

	return request, Error.Wrap(db.db.View(func(txn *badger.Txn) error {
		// TODO(artur): to ensure a lower load on the backing store, consider
		// reading available clocks only once and maintaining a cache of clocks.
		availableClocks, err := readAvailableClocks(txn)
		if err != nil {
			return err
		}

		// We don't need the local node ID in the replication request.
		delete(availableClocks, db.config.ID)

		for id, clock := range availableClocks {
			request = append(request, &pb.ReplicationRequestEntry{
				NodeId: id.Bytes(),
				Clock:  uint64(clock),
			})
		}

		return nil
	}))
}

func (db *DB) insertResponseEntries(ctx context.Context, response *pb.ReplicationResponse) (err error) {
	defer mon.Task()(&ctx)(&err)

	return Error.Wrap(db.txnWithBackoff(ctx, func(txn *badger.Txn) error {
		for i, entry := range response.Entries {
			var (
				id      NodeID
				keyHash authdb.KeyHash
			)

			if err = id.SetBytes(entry.NodeId); err != nil {
				return err
			}
			// TODO(artur): authdb.KeyHash needs the same abstraction as NodeID.
			copy(keyHash[:], entry.EncryptionKeyHash)

			if err = InsertRecord(txn, id, keyHash, entry.Record); err != nil {
				return errs.New("failed to insert entry no. %d (%v) from %v: %w", i, keyHash, id, err)
			}
		}
		return nil
	}))
}

func (db *DB) eventTags(a action) []monkit.SeriesTag {
	return []monkit.SeriesTag{
		monkit.NewSeriesTag("action", a.String()),
		monkit.NewSeriesTag("node_id", db.config.ID.String()),
	}
}

func (db *DB) monitorEvent(name string, a action, tags ...monkit.SeriesTag) {
	mon.Event("as_badgerauth_"+name, db.eventTags(a)...)
}

// InsertRecord inserts a record, adding a corresponding replication log entry
// consistent with the record's state.
//
// InsertRecord can be used to insert on any node for any node.
func InsertRecord(txn *badger.Txn, nodeID NodeID, keyHash authdb.KeyHash, record *pb.Record) error {
	if record.State != pb.Record_CREATED {
		return errOperationNotSupported
	}
	// NOTE(artur): the check below is a sanity check (generally, this shouldn't
	// happen because access key hashes are unique) that can be slurped into the
	// replication process itself if needed.
	if i, err := txn.Get(keyHash[:]); err == nil {
		var loaded pb.Record

		if err = i.Value(func(val []byte) error {
			return pb.Unmarshal(val, &loaded)
		}); err != nil {
			return Error.Wrap(ProtoError.Wrap(err))
		}

		if !recordsEqual(record, &loaded) {
			return errKeyAlreadyExistsRecordsNotEqual
		}
	} else if !errs.Is(err, badger.ErrKeyNotFound) {
		return Error.Wrap(err)
	}

	marshaled, err := pb.Marshal(record)
	if err != nil {
		return Error.Wrap(ProtoError.Wrap(err))
	}

	clock, err := advanceClock(txn, nodeID) // vector clock for this operation
	if err != nil {
		return Error.Wrap(err)
	}

	mainEntry := badger.NewEntry(keyHash[:], marshaled)
	rlogEntry := ReplicationLogEntry{
		ID:      nodeID,
		Clock:   clock,
		KeyHash: keyHash,
		State:   record.State,
	}.ToBadgerEntry()

	mainEntry.ExpiresAt = uint64(record.ExpiresAtUnix)
	rlogEntry.ExpiresAt = uint64(record.ExpiresAtUnix)

	return Error.Wrap(errs.Combine(txn.SetEntry(mainEntry), txn.SetEntry(rlogEntry)))
}

func lookupRecord(txn *badger.Txn, keyHash authdb.KeyHash) (*pb.Record, error) {
	var record pb.Record

	item, err := txn.Get(keyHash[:])
	if err != nil {
		return nil, err
	}

	return &record, item.Value(func(val []byte) error {
		return ProtoError.Wrap(pb.Unmarshal(val, &record))
	})
}
