package indexwrapper

import (
	"github.com/RoaringBitmap/roaring"
	"github.com/matrixorigin/matrixone/pkg/container/vector"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/catalog"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/iface/data"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/index"
)

type immutableIndex struct {
	zmReader *ZMReader
	bfReader *BFReader
}

func NewImmutableIndex() *immutableIndex {
	return new(immutableIndex)
}

func (index *immutableIndex) IsKeyDeleted(any, uint64) (bool, bool) { panic("not supported") }
func (index *immutableIndex) GetActiveRow(any) (uint32, error)      { panic("not supported") }
func (index *immutableIndex) Delete(any, uint64) error              { panic("not supported") }
func (index *immutableIndex) BatchUpsert(*index.KeysCtx, uint32, uint64) error {
	panic("not supported")
}

func (index *immutableIndex) Dedup(key any) (err error) {
	exist := index.zmReader.Contains(key)
	// 2. if not in [min, max], key is definitely not found
	if !exist {
		return
	}
	exist, err = index.bfReader.MayContainsKey(key)
	// 3. check bloomfilter has some error. return err
	if err != nil {
		err = TranslateError(err)
		return
	}
	if exist {
		err = data.ErrPossibleDuplicate
	}
	return
}

func (index *immutableIndex) BatchDedup(keys *vector.Vector, rowmask *roaring.Bitmap) (keyselects *roaring.Bitmap, err error) {
	keyselects, exist := index.zmReader.ContainsAny(keys)
	// 1. all keys are not in [min, max]. definitely not
	if !exist {
		return
	}
	exist, keyselects, err = index.bfReader.MayContainsAnyKeys(keys, keyselects)
	// 3. check bloomfilter has some unknown error. return err
	if err != nil {
		err = TranslateError(err)
		return
	}
	// 4. all keys were checked. definitely not
	if !exist {
		return
	}
	err = data.ErrPossibleDuplicate
	return
}

func (index *immutableIndex) Close() (err error) {
	// TODO
	return
}

func (index *immutableIndex) Destroy() (err error) {
	if err = index.zmReader.Destroy(); err != nil {
		return
	}
	err = index.bfReader.Destroy()
	return
}

func (index *immutableIndex) ReadFrom(blk data.Block) (err error) {
	file := blk.GetBlockFile()
	idxMeta, err := file.LoadIndexMeta()
	if err != nil {
		return
	}
	metas := idxMeta.(*IndicesMeta)
	entry := blk.GetMeta().(*catalog.BlockEntry)
	colFile, err := file.OpenColumn(int(entry.GetSchema().PrimaryKey))
	if err != nil {
		return
	}
	for _, meta := range metas.Metas {
		idxFile, err := colFile.OpenIndexFile(int(meta.InternalIdx))
		if err != nil {
			return err
		}
		id := entry.AsCommonID()
		id.PartID = uint32(meta.InternalIdx) + 1000
		id.Idx = meta.ColIdx
		switch meta.IdxType {
		case BlockZoneMapIndex:
			size := idxFile.Stat().Size()
			buf := make([]byte, size)
			if _, err = idxFile.Read(buf); err != nil {
				return err
			}
			index.zmReader = NewZMReader(blk.GetBufMgr(), idxFile, id)
		case StaticFilterIndex:
			size := idxFile.Stat().Size()
			buf := make([]byte, size)
			if _, err = idxFile.Read(buf); err != nil {
				return err
			}
			index.bfReader = NewBFReader(blk.GetBufMgr(), idxFile, id)
		default:
			panic("unsupported index type")
		}
	}
	return
}

func (index *immutableIndex) WriteTo(data.Block) error { panic("not supported") }
