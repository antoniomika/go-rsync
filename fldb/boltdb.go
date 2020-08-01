package fldb

import (
	"github.com/golang/protobuf/proto"
	bolt "go.etcd.io/bbolt"
	"rsync-os/rsync"
)

type BoltDB struct {
	db      *bolt.DB
	module  []byte
	prepath []byte
}

func Open(path string, module []byte, prepath []byte) *BoltDB {
	db, err := bolt.Open(path, 0666, nil)
	if err != nil {
		panic("Can't init fldb: boltdb")
	}
	return &BoltDB{
		db:      db,
		module:  module,
		prepath: prepath,
	}
}

func (c *BoltDB)Close() {
	c.db.Close()
}


func (cache *BoltDB) Update(list rsync.FileList, downloadList []int, deleteList [][]byte) error {
	err := cache.db.Update(func(tx *bolt.Tx) error {
		mod := tx.Bucket(cache.module)
		// If bucket does not exist, create the bucket
		if mod == nil {
			panic("Bucket should be created")
		}

		// Insert new items in cache
		for _, idx := range downloadList {
			info := list[idx]
			key := append(cache.prepath, info.Path...)
			value, err := proto.Marshal(&FInfo{
				Size:  info.Size,
				Mtime: info.Mtime,
				Mode:  int32(info.Mode),
			})
			if err != nil {
				return err
			}
			return mod.Put(key, value)
		}

		// Remove items in cache
		for _, rkey := range deleteList {
			err := mod.Delete(rkey)
			if err != nil {
				return err
			}
		}

		return nil
	})

	return err
}

//func (cache *BoltDB) PutAll(list *rsync.FileList) error {
//	for _, info := range *list {
//		err := cache.Put(&info)
//		if err != nil {
//			return err
//		}
//	}
//	return nil
//}
