package filer

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	"github.com/chrislusf/seaweedfs/weed/util"
	"github.com/chrislusf/seaweedfs/weed/util/log_buffer"
	"github.com/chrislusf/seaweedfs/weed/wdclient"
)

const (
	LogFlushInterval = time.Minute
	PaginationSize   = 1024
	FilerStoreId     = "filer.store.id"
)

var (
	OS_UID = uint32(os.Getuid())
	OS_GID = uint32(os.Getgid())
)

type Filer struct {
	Store               VirtualFilerStore
	MasterClient        *wdclient.MasterClient
	fileIdDeletionQueue *util.UnboundedQueue
	GrpcDialOption      grpc.DialOption
	DirBucketsPath      string
	FsyncBuckets        []string
	buckets             *FilerBuckets
	Cipher              bool
	LocalMetaLogBuffer  *log_buffer.LogBuffer
	metaLogCollection   string
	metaLogReplication  string
	MetaAggregator      *MetaAggregator
	Signature           int32
	FilerConf           *FilerConf
}

func NewFiler(masters []string, grpcDialOption grpc.DialOption,
	filerHost string, filerGrpcPort uint32, collection string, replication string, dataCenter string, notifyFn func()) *Filer {
	f := &Filer{
		MasterClient:        wdclient.NewMasterClient(grpcDialOption, "filer", filerHost, filerGrpcPort, dataCenter, masters),
		fileIdDeletionQueue: util.NewUnboundedQueue(),
		GrpcDialOption:      grpcDialOption,
		FilerConf:           NewFilerConf(),
	}
	f.LocalMetaLogBuffer = log_buffer.NewLogBuffer(LogFlushInterval, f.logFlushFunc, notifyFn)
	f.metaLogCollection = collection
	f.metaLogReplication = replication

	go f.loopProcessingDeletion()

	return f
}

func (f *Filer) AggregateFromPeers(self string, filers []string) {

	// set peers
	found := false
	for _, peer := range filers {
		if peer == self {
			found = true
		}
	}
	if !found {
		filers = append(filers, self)
	}

	f.MetaAggregator = NewMetaAggregator(filers, f.GrpcDialOption)
	f.MetaAggregator.StartLoopSubscribe(f, self)

}

func (f *Filer) SetStore(store FilerStore) {
	f.Store = NewFilerStoreWrapper(store)

	f.setOrLoadFilerStoreSignature(store)

}

func (f *Filer) setOrLoadFilerStoreSignature(store FilerStore) {
	storeIdBytes, err := store.KvGet(context.Background(), []byte(FilerStoreId))
	if err == ErrKvNotFound || err == nil && len(storeIdBytes) == 0 {
		f.Signature = util.RandomInt32()
		storeIdBytes = make([]byte, 4)
		util.Uint32toBytes(storeIdBytes, uint32(f.Signature))
		if err = store.KvPut(context.Background(), []byte(FilerStoreId), storeIdBytes); err != nil {
			glog.Fatalf("set %s=%d : %v", FilerStoreId, f.Signature, err)
		}
		glog.V(0).Infof("create %s to %d", FilerStoreId, f.Signature)
	} else if err == nil && len(storeIdBytes) == 4 {
		f.Signature = int32(util.BytesToUint32(storeIdBytes))
		glog.V(0).Infof("existing %s = %d", FilerStoreId, f.Signature)
	} else {
		glog.Fatalf("read %v=%v : %v", FilerStoreId, string(storeIdBytes), err)
	}
}

func (f *Filer) GetStore() (store FilerStore) {
	return f.Store
}

func (fs *Filer) GetMaster() string {
	return fs.MasterClient.GetMaster()
}

func (fs *Filer) KeepConnectedToMaster() {
	fs.MasterClient.KeepConnectedToMaster()
}

func (f *Filer) BeginTransaction(ctx context.Context) (context.Context, error) {
	return f.Store.BeginTransaction(ctx)
}

func (f *Filer) CommitTransaction(ctx context.Context) error {
	return f.Store.CommitTransaction(ctx)
}

func (f *Filer) RollbackTransaction(ctx context.Context) error {
	return f.Store.RollbackTransaction(ctx)
}

func (f *Filer) CreateEntry(ctx context.Context, entry *Entry, o_excl bool, isFromOtherCluster bool, signatures []int32) error {

	if string(entry.FullPath) == "/" {
		return nil
	}

	oldEntry, _ := f.FindEntry(ctx, entry.FullPath)

	/*
		if !hasWritePermission(lastDirectoryEntry, entry) {
			glog.V(0).Infof("directory %s: %v, entry: uid=%d gid=%d",
				lastDirectoryEntry.FullPath, lastDirectoryEntry.Attr, entry.Uid, entry.Gid)
			return fmt.Errorf("no write permission in folder %v", lastDirectoryEntry.FullPath)
		}
	*/

	if oldEntry == nil {

		dirParts := strings.Split(string(entry.FullPath), "/")
		if err := f.ensureParentDirecotryEntry(ctx, entry, dirParts, len(dirParts)-1, isFromOtherCluster); err != nil {
			return err
		}

		glog.V(4).Infof("InsertEntry %s: new entry: %v", entry.FullPath, entry.Name())
		if err := f.Store.InsertEntry(ctx, entry); err != nil {
			glog.Errorf("insert entry %s: %v", entry.FullPath, err)
			return fmt.Errorf("insert entry %s: %v", entry.FullPath, err)
		}
	} else {
		if o_excl {
			glog.V(3).Infof("EEXIST: entry %s already exists", entry.FullPath)
			return fmt.Errorf("EEXIST: entry %s already exists", entry.FullPath)
		}
		glog.V(4).Infof("UpdateEntry %s: old entry: %v", entry.FullPath, oldEntry.Name())
		if err := f.UpdateEntry(ctx, oldEntry, entry); err != nil {
			glog.Errorf("update entry %s: %v", entry.FullPath, err)
			return fmt.Errorf("update entry %s: %v", entry.FullPath, err)
		}
	}

	f.maybeAddBucket(entry)
	f.NotifyUpdateEvent(ctx, oldEntry, entry, true, isFromOtherCluster, signatures)

	f.deleteChunksIfNotNew(oldEntry, entry)

	glog.V(4).Infof("CreateEntry %s: created", entry.FullPath)

	return nil
}

func (f *Filer) ensureParentDirecotryEntry(ctx context.Context, entry *Entry, dirParts []string, level int, isFromOtherCluster bool) (err error) {

	if level == 0 {
		return nil
	}

	dirPath := "/" + util.Join(dirParts[:level]...)
	// fmt.Printf("%d directory: %+v\n", i, dirPath)

	// check the store directly
	glog.V(4).Infof("find uncached directory: %s", dirPath)
	dirEntry, _ := f.FindEntry(ctx, util.FullPath(dirPath))

	// no such existing directory
	if dirEntry == nil {

		// ensure parent directory
		if err = f.ensureParentDirecotryEntry(ctx, entry, dirParts, level-1, isFromOtherCluster); err != nil {
			return err
		}

		// create the directory
		now := time.Now()

		dirEntry = &Entry{
			FullPath: util.FullPath(dirPath),
			Attr: Attr{
				Mtime:       now,
				Crtime:      now,
				Mode:        os.ModeDir | entry.Mode | 0110,
				Uid:         entry.Uid,
				Gid:         entry.Gid,
				Collection:  entry.Collection,
				Replication: entry.Replication,
				UserName:    entry.UserName,
				GroupNames:  entry.GroupNames,
			},
		}

		glog.V(2).Infof("create directory: %s %v", dirPath, dirEntry.Mode)
		mkdirErr := f.Store.InsertEntry(ctx, dirEntry)
		if mkdirErr != nil {
			if _, err := f.FindEntry(ctx, util.FullPath(dirPath)); err == filer_pb.ErrNotFound {
				glog.V(3).Infof("mkdir %s: %v", dirPath, mkdirErr)
				return fmt.Errorf("mkdir %s: %v", dirPath, mkdirErr)
			}
		} else {
			f.maybeAddBucket(dirEntry)
			f.NotifyUpdateEvent(ctx, nil, dirEntry, false, isFromOtherCluster, nil)
		}

	} else if !dirEntry.IsDirectory() {
		glog.Errorf("CreateEntry %s: %s should be a directory", entry.FullPath, dirPath)
		return fmt.Errorf("%s is a file", dirPath)
	}

	return nil
}

func (f *Filer) UpdateEntry(ctx context.Context, oldEntry, entry *Entry) (err error) {
	if oldEntry != nil {
		entry.Attr.Crtime = oldEntry.Attr.Crtime
		if oldEntry.IsDirectory() && !entry.IsDirectory() {
			glog.Errorf("existing %s is a directory", entry.FullPath)
			return fmt.Errorf("existing %s is a directory", entry.FullPath)
		}
		if !oldEntry.IsDirectory() && entry.IsDirectory() {
			glog.Errorf("existing %s is a file", entry.FullPath)
			return fmt.Errorf("existing %s is a file", entry.FullPath)
		}
	}
	return f.Store.UpdateEntry(ctx, entry)
}

var (
	Root = &Entry{
		FullPath: "/",
		Attr: Attr{
			Mtime:  time.Now(),
			Crtime: time.Now(),
			Mode:   os.ModeDir | 0755,
			Uid:    OS_UID,
			Gid:    OS_GID,
		},
	}
)

func (f *Filer) FindEntry(ctx context.Context, p util.FullPath) (entry *Entry, err error) {

	if string(p) == "/" {
		return Root, nil
	}
	entry, err = f.Store.FindEntry(ctx, p)
	if entry != nil && entry.TtlSec > 0 {
		if entry.Crtime.Add(time.Duration(entry.TtlSec) * time.Second).Before(time.Now()) {
			f.Store.DeleteOneEntry(ctx, entry)
			return nil, filer_pb.ErrNotFound
		}
	}
	return

}

func (f *Filer) doListDirectoryEntries(ctx context.Context, p util.FullPath, startFileName string, inclusive bool, limit int64, prefix string, eachEntryFunc ListEachEntryFunc) (expiredCount int64, lastFileName string, err error) {
	lastFileName, err = f.Store.ListDirectoryPrefixedEntries(ctx, p, startFileName, inclusive, limit, prefix, func(entry *Entry) bool {
		if entry.TtlSec > 0 {
			if entry.Crtime.Add(time.Duration(entry.TtlSec) * time.Second).Before(time.Now()) {
				f.Store.DeleteOneEntry(ctx, entry)
				expiredCount++
				return true
			}
		}
		return eachEntryFunc(entry)
	})
	if err != nil {
		return expiredCount, lastFileName, err
	}
	return
}

func (f *Filer) Shutdown() {
	f.LocalMetaLogBuffer.Shutdown()
	f.Store.Shutdown()
}
