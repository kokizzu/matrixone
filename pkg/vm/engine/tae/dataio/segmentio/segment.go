// Copyright 2021 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package segmentio

import (
	"bytes"
	"fmt"
	"github.com/matrixorigin/matrixone/pkg/logutil"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/common"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/iface/file"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/layout/segment"
	"strconv"
	"strings"
	"sync"
)

var SegmentFileIOFactory = func(name string, id uint64) file.Segment {
	return newSegmentFile(name, id)
}

var SegmentFileIOOpenFactory = func(name string, id uint64) file.Segment {
	return openSegment(name, id)
}

type segmentFile struct {
	sync.RWMutex
	common.RefHelper
	id     *common.ID
	ts     uint64
	blocks map[uint64]*blockFile
	name   string
	seg    *segment.Segment
}

func openSegment(name string, id uint64) *segmentFile {
	sf := &segmentFile{
		blocks: make(map[uint64]*blockFile),
		name:   name,
	}
	sf.seg = &segment.Segment{}
	err := sf.seg.Open(sf.name)
	if err != nil {
		return nil
	}
	sf.id = &common.ID{
		SegmentID: id,
	}
	sf.Ref()
	sf.OnZeroCB = sf.close
	return sf
}

func (sf *segmentFile) RemoveBlock(id uint64) {
	sf.Lock()
	defer sf.Unlock()
	block := sf.blocks[id]
	if block == nil {
		return
	}
	delete(sf.blocks, id)
}

func (sf *segmentFile) Replay(colCnt int, indexCnt map[int]int, cache *bytes.Buffer) error {
	err := sf.seg.Replay(cache)
	if err != nil {
		return err
	}
	nodes := sf.seg.GetNodes()
	sf.Lock()
	defer sf.Unlock()
	for name, file := range nodes {
		tmpName := strings.Split(name, ".blk")
		fileName := strings.Split(tmpName[0], "_")
		if len(fileName) < 2 {
			continue
		}
		id, err := strconv.ParseUint(fileName[1], 10, 32)
		if err != nil {
			return err
		}
		bf := sf.blocks[id]
		if bf == nil {
			bf = replayBlock(id, sf, colCnt, indexCnt)
			sf.blocks[id] = bf
		}
		col, err := strconv.ParseUint(fileName[0], 10, 32)
		if err != nil {
			return err
		}
		bf.columns[col].data.file = append(bf.columns[col].data.file, file)
		bf.columns[col].data.stat.size = file.GetFileSize()
		bf.columns[col].data.stat.originSize = file.GetOriginSize()
		bf.columns[col].data.stat.algo = file.GetAlgo()
		bf.columns[col].data.stat.name = file.GetName()
		if len(fileName) > 2 {
			ts, err := strconv.ParseUint(fileName[2], 10, 64)
			if err != nil {
				return err
			}
			if bf.columns[col].ts < ts {
				bf.columns[col].ts = ts
			}
		}

	}
	return nil
}

func newSegmentFile(name string, id uint64) *segmentFile {
	sf := &segmentFile{
		blocks: make(map[uint64]*blockFile),
		name:   name,
	}
	sf.seg = &segment.Segment{}
	err := sf.seg.Init(sf.name)
	if err != nil {
		return nil
	}
	sf.seg.Mount()
	sf.id = &common.ID{
		SegmentID: id,
	}
	sf.Ref()
	sf.OnZeroCB = sf.close
	return sf
}

func (sf *segmentFile) Fingerprint() *common.ID { return sf.id }
func (sf *segmentFile) Close() error            { return nil }

func (sf *segmentFile) close() {
	sf.Destroy()
}
func (sf *segmentFile) Destroy() {
	logutil.Infof("Destroying Segment %d", sf.id.SegmentID)
	sf.RLock()
	blocks := sf.blocks
	sf.RUnlock()
	for _, block := range blocks {
		block.Destroy()
	}
	sf.seg.Unmount()
	sf.seg.Destroy()
}

func (sf *segmentFile) OpenBlock(id uint64, colCnt int, indexCnt map[int]int) (block file.Block, err error) {
	sf.Lock()
	defer sf.Unlock()
	bf := sf.blocks[id]
	if bf == nil {
		bf = newBlock(id, sf, colCnt, indexCnt)
		sf.blocks[id] = bf
	}
	block = bf
	return
}

func (sf *segmentFile) WriteTS(ts uint64) error {
	sf.ts = ts
	return nil
}

func (sf *segmentFile) ReadTS() uint64 {
	return sf.ts
}

func (sf *segmentFile) String() string {
	s := fmt.Sprintf("SegmentFile[%d][\"%s\"][TS=%d][BCnt=%d]", sf.id, sf.name, sf.ts, len(sf.blocks))
	return s
}

func (sf *segmentFile) GetSegmentFile() *segment.Segment {
	return sf.seg
}

func (sf *segmentFile) Sync() error {
	return sf.seg.Sync()
}
