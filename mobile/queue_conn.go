package webrtcLib

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
)

const (
	QCDataVideoOne QCDataTye = iota + 1
	QCDataVideoTwo
	QCDataAudio
	QCDataNack
)

const (
	QCIceMtu    = 1 << 13
	QCSliceSize = QCIceMtu - QCHeaderSize

	QCDataTypeLen = 1
	QCSequenceLen = 4
	QCHeaderSize  = QCDataTypeLen + QCSequenceLen

	QCNodePool    = 1 << 12
	QCNullPointer = -1

	QCSliceToWait     = 1 << 3
	QCSliceLostToSkip = 1 << 4
)

var (
	QCErrDataLost    = fmt.Errorf("data lost")
	QCErrHeaderLost  = fmt.Errorf("header lost")
	QCErrAckLost     = fmt.Errorf("ack lost")
	QCErrCacheLost   = fmt.Errorf("cache lost")
	QCErrDataInvalid = fmt.Errorf("data type unknown")
)

type QCDataTye byte

func (t QCDataTye) String() string {
	switch t {
	case QCDataVideoOne:
		return "video1"
	case QCDataVideoTwo:
		return "video2"
	case QCDataAudio:
		return "audio"
	case QCDataNack:
		return "nack"
	default:
		return "unknown"
	}
}

type QueueConn struct {
	conn net.Conn

	seq uint32

	sendCache [][]byte
	rendBuf   chan []byte

	rcvPool *SortedQueue
	rcvSig  chan struct{}
}

func NewQueueConn(c net.Conn) *QueueConn {
	return &QueueConn{
		conn: c,
		rcvPool: &SortedQueue{
			Pointer: QCNullPointer,
			Pool:    make([]*DataNode, QCNodePool),
		},
		rcvSig:    make(chan struct{}, QCNodePool),
		sendCache: make([][]byte, QCNodePool),
		rendBuf:   make(chan []byte, QCNodePool),
	}
}

func (qc *QueueConn) Close() {
	if qc.conn == nil {
		return
	}
	qc.rcvPool.Reset()
	qc.seq = 0
	_ = qc.conn.Close()
	close(qc.rcvSig)
	close(qc.rendBuf)
	qc.conn = nil
}

func (qc *QueueConn) sendWithSeqAndTyp(typ QCDataTye, buf []byte) error {
	var dataLen = len(buf)
	fmt.Println("\t\t\t\t\t\t\t\t\t\t\t\t======>>>frame data=> type:", typ.String(),
		" length:", dataLen) //, hex.EncodeToString(buf))

	for startIdx := 0; startIdx < dataLen; startIdx = startIdx + QCSliceSize {

		var endIdx = startIdx + QCSliceSize
		var sliceLen = QCSliceSize
		if endIdx > dataLen {
			endIdx = dataLen
			sliceLen = dataLen - startIdx
		}

		atomic.AddUint32(&qc.seq, 1)

		var headBuf = make([]byte, QCHeaderSize)
		binary.BigEndian.PutUint32(headBuf[:QCSequenceLen], qc.seq)
		headBuf[QCSequenceLen] = byte(typ)

		var ackIdx = qc.seq % QCNodePool
		var data = append(headBuf, buf[startIdx:endIdx]...)
		qc.sendCache[ackIdx] = data

		var n, errW = qc.conn.Write(data)
		if errW != nil {
			return errW
		}
		if n != sliceLen+QCHeaderSize {
			fmt.Println("\t\t\t\t\t\t\t\t\t\t\t\t======>>>qc write err:", n, hex.EncodeToString(headBuf), hex.EncodeToString(data), sliceLen, QCHeaderSize)
			return QCErrDataLost
		}
		fmt.Println("\t\t\t\t\t\t\t\t\t\t\t\t======>>>qc write=> seq:",
			qc.seq, " Typ:", typ.String(),
			" slice size:", sliceLen, " cache idx:", ackIdx)
		//,			" data len:", len(qc.sendCache[ackIdx]))
	}

	return nil
}

func (qc *QueueConn) readingLostSeq(errCh chan error) {
	for {
		var node, err = qc.readDataNodeFromPeer()
		if err != nil {
			errCh <- err
			return
		}
		err = qc.resendLostPkt(node)
		if err != nil {
			if errors.Is(err, QCErrCacheLost) {
				continue
			}
			errCh <- err
			return
		}
	}
}

func (qc *QueueConn) WritingFrame(typ QCDataTye, dataSource func() ([]byte, error), errCh chan error) {

	for {
		select {
		case lostData := <-qc.rendBuf:
			var _, errW = qc.conn.Write(lostData)
			if errW != nil {
				errCh <- errW
				return
			}
			continue
		default:
		}

		var buf, err = dataSource()
		if err != nil {
			errCh <- err
			return
		}
		//fmt.Println("======>>> device data", hex.EncodeToString(buf))
		err = qc.sendWithSeqAndTyp(typ, buf)
		if err != nil {
			errCh <- err
			return
		}
	}
}

func (qc *QueueConn) readDataNodeFromPeer() (*DataNode, error) {

	var buffer = make([]byte, QCIceMtu)
	var n, err = qc.conn.Read(buffer)
	if err != nil {
		return nil, err
	}
	if n < QCHeaderSize {
		fmt.Println("******>>>read invalid data:", n, hex.EncodeToString(buffer[:n]))
		return nil, QCErrHeaderLost
	}

	var seq = binary.BigEndian.Uint32(buffer[:QCSequenceLen])
	var dataTyp = QCDataTye(buffer[QCSequenceLen:QCHeaderSize][0])
	buffer = buffer[QCHeaderSize:n]

	var node = &DataNode{}

	switch dataTyp {
	case QCDataNack:
		node.Typ = QCDataNack
		node.Buf = buffer

	case QCDataVideoOne, QCDataVideoTwo:
		node.Typ = dataTyp
		node.Buf = buffer
		node.Seq = seq

		var videoStartIdx = bytes.Index(buffer[:VideoAvcLen], VideoAvcStart)
		node.IsKey = videoStartIdx == 0
	case QCDataAudio:
		node.Typ = dataTyp
		node.Buf = buffer
		node.Seq = seq
	default:
		return nil, QCErrDataInvalid
	}

	fmt.Println("******>>>qc read node:", node.String())
	return node, nil
}

func (qc *QueueConn) resendLostPkt(node *DataNode) error {
	var dataLen = len(node.Buf)
	if dataLen != QCSequenceLen {
		return QCErrAckLost
	}

	var ackIdx = binary.BigEndian.Uint32(node.Buf)
	var idxInCache = ackIdx % QCNodePool

	var buf = qc.sendCache[idxInCache]
	if buf == nil {
		fmt.Println("\t\t\t\t\t\t\t\t\t\t\t\t&&&&&&&&&&&&&&&&&&&&&>>>"+
			" lost payload not found:", idxInCache, ackIdx, qc.seq)
		return QCErrCacheLost
	}

	fmt.Println("\t\t\t\t\t\t\t\t\t\t\t\t&&&&&&&&&&&&&&&&&&&&&>>>resending lost pkt seq:", ackIdx)

	qc.rendBuf <- buf
	return nil
}

func (qc *QueueConn) reading(sig chan struct{}, eCh chan error) {
	for {
		var node, err = qc.readDataNodeFromPeer()
		if err != nil {
			eCh <- err
			return
		}
		_ = qc.rcvPool.Product(node)
		sig <- struct{}{}
		continue
	}
}

func (qc *QueueConn) ReadFrameData(bufCh chan []byte) error {

	var errCh = make(chan error, 2)
	var lostSeq = make(chan uint32, QCNodePool)

	go qc.reading(qc.rcvSig, errCh)

	for {
		select {
		case seq := <-lostSeq:
			fmt.Println("&&&&&&&&&&&&&&&&&&&&&>>> require to resend seq:", seq)

			var buf = make([]byte, QCSequenceLen)
			binary.BigEndian.PutUint32(buf[:QCSequenceLen], seq)
			var err = qc.sendWithSeqAndTyp(QCDataNack, buf)

			if err != nil {
				return err
			}

			continue
		case <-qc.rcvSig:
			var buf = qc.rcvPool.Consume(lostSeq)
			if buf == nil {
				continue
			}
			//fmt.Println("******>>>got remote data:", hex.EncodeToString(buf))
			bufCh <- buf
		case e := <-errCh:
			return e
		}
	}
}

type DataNode struct {
	Typ   QCDataTye
	Seq   uint32
	Buf   []byte
	IsKey bool
}

func (dn *DataNode) String() string {
	var s = fmt.Sprintf("{Seq:%d\t", dn.Seq)
	s += fmt.Sprintf("Typ:%s\t", dn.Typ.String())
	s += fmt.Sprintf("IsKey:%t\t", dn.IsKey)
	s += fmt.Sprintf("Buf:%d}", len(dn.Buf))
	return s
}

func (dn *DataNode) isEmpty() bool {
	return dn.Seq == 0 || len(dn.Buf) == 0
}

type SortedQueue struct {
	sync.RWMutex
	Pointer int
	Pool    []*DataNode

	lostCounter    int
	timeoutCounter int
}

func (dp *SortedQueue) Reset() {
	dp.Lock()
	dp.Unlock()

	dp.Pool = nil
	dp.Pointer = QCNullPointer
}

func (dp *SortedQueue) skipToNextFrameWithoutLock() {
	dp.lostCounter = 0
	dp.timeoutCounter = 0
	for i := 2; i < QCNodePool; i++ {
		var nextPos = (dp.Pointer + i) % QCNodePool
		var next = dp.Pool[nextPos]
		if next == nil {
			continue
		}
		if !next.IsKey {
			continue
		}
		dp.Pointer = nextPos
		fmt.Println("&&&&&&&&&&&&&&&&&&&&&>>> skip to next frame:", next.String())
		return
	}
}
func (dp *SortedQueue) findNext(lostSeq chan uint32, cur *DataNode) {
	dp.Lock()
	defer dp.Unlock()

	dp.lostCounter++
	dp.timeoutCounter++

	fmt.Println("\t\t\t\t\t\t------>>> no next node :", cur.String(),
		" lost counter:", dp.lostCounter,
		" timeout counter:", dp.timeoutCounter)

	if dp.lostCounter > QCSliceToWait {
		if dp.timeoutCounter <= QCSliceLostToSkip {
			fmt.Println("\t\t\t\t\t\t&&&&&&&&&&>>> seq lost:", cur.Seq+1)
			dp.lostCounter = 0
			lostSeq <- cur.Seq + 1
		} else {
			dp.skipToNextFrameWithoutLock()
		}
	}
}

func (dp *SortedQueue) Consume(lostSeq chan uint32) []byte {
	dp.RLock()
	if dp.Pointer == QCNullPointer {
		//fmt.Println("------>>> empty queue:")
		dp.RUnlock()
		return nil
	}
	var cur = dp.Pool[dp.Pointer]
	if cur == nil {
		//fmt.Println("------>>> empty queue:")
		dp.RUnlock()
		return nil
	}

	var nextPos = (dp.Pointer + 1) % QCNodePool
	var next = dp.Pool[nextPos]
	if next == nil {
		dp.RUnlock()
		dp.findNext(lostSeq, cur)
		return nil
	}

	dp.RUnlock()

	dp.Lock()
	defer dp.Unlock()
	if !next.IsKey {
		fmt.Println("\t\t\t\t\t\t------>>> merge node cur:", cur.String(),
			" next:", next.String())
		next.Buf = append(cur.Buf, next.Buf...)
		next.IsKey = cur.IsKey
		dp.Pool[dp.Pointer] = nil
		dp.Pointer = nextPos
		fmt.Println("\t\t\t\t\t\t------>>> next now:", next.String())
		return nil
	}

	fmt.Println("\t\t\t\t\t\t--------------------------------------->>> key node", cur.String(),
		" next:", next.String())
	if !cur.IsKey {
		dp.skipToNextFrameWithoutLock()
		return nil
	}

	dp.Pool[dp.Pointer] = nil
	dp.Pointer = nextPos
	return cur.Buf
}

func (dp *SortedQueue) Product(node *DataNode) error {
	if node == nil {
		return fmt.Errorf("empty data")
	}
	dp.Lock()
	defer dp.Unlock()
	if dp.Pool == nil {
		return fmt.Errorf("empty pool")
	}

	var pos = int(node.Seq % QCNodePool)
	dp.Pool[pos] = node
	if dp.Pointer == QCNullPointer && node.Seq == 1 {
		fmt.Println("\t\t\t\t\t\t------>>> found start seq:", node.String())
		dp.Pointer = pos
	}
	return nil
}
