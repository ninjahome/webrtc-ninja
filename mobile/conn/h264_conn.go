package conn

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
)

const (
	FrameStackSize = 1 << 6
	NinHeaderLen   = 8
)

var (
	NCInvalidVideoFrame = fmt.Errorf("invalid ninja video frame info")
	NCOneBadFrameData   = fmt.Errorf("bad frame data")
)

type H264Conn struct {
	connReader   io.Reader
	connWriter   io.Writer
	frameCounter uint16
	frameStack   [FrameStackSize]*ReceiveFrame
	sliceCache   chan *Slice
}

func NewH264Conn(reader io.Reader, writer io.Writer) *H264Conn {
	return &H264Conn{
		connReader: reader,
		connWriter: writer,
	}
}

type VideoFrame struct {
	FrameID    uint16
	SliceCount uint16
	CurSliceID uint16
	CurLen     uint16
}

func (f *VideoFrame) Data() []byte {
	var frameBuf = make([]byte, 8)
	binary.BigEndian.PutUint16(frameBuf[:2], f.FrameID)
	binary.BigEndian.PutUint16(frameBuf[2:4], f.SliceCount)
	binary.BigEndian.PutUint16(frameBuf[4:6], f.CurSliceID)
	binary.BigEndian.PutUint16(frameBuf[6:8], f.CurLen)
	return frameBuf
}

func (f *VideoFrame) String() any {
	var s = fmt.Sprintf("{frame id:%d\t", f.FrameID)
	s = s + fmt.Sprintf("slice count:%d\t", f.SliceCount)
	s = s + fmt.Sprintf("current slice:%d\t", f.CurSliceID)
	s = s + fmt.Sprintf("payload:%d}", f.CurLen)
	return s
}

type Slice struct {
	Header  *VideoFrame
	Payload []byte
}

type ReceiveFrame struct {
	FrameID     uint16
	SliceHasGot uint16
	HasFinished bool
	Cache       []*Slice
}

func (rf *ReceiveFrame) Flush() []byte {
	var buf []byte
	for _, slice := range rf.Cache {
		buf = append(buf, slice.Payload...)
	}
	return buf
}

func (rf *ReceiveFrame) String() any {
	var s = fmt.Sprintf("{frame id:%d\t", rf.FrameID)
	s = s + fmt.Sprintf("slice got:%d\t", rf.SliceHasGot)
	s = s + fmt.Sprintf("has finished:%t\t", rf.HasFinished)
	s = s + fmt.Sprintf("cache size:%d}", len(rf.Cache))
	return s
}

func ParseFrame(frame *VideoFrame, data []byte) error {
	frame.FrameID = binary.BigEndian.Uint16(data[:2])
	frame.SliceCount = binary.BigEndian.Uint16(data[2:4])
	frame.CurSliceID = binary.BigEndian.Uint16(data[4:6])
	frame.CurLen = binary.BigEndian.Uint16(data[6:8])
	if frame.CurLen > IceUdpMtu ||
		frame.SliceCount == 0 ||
		frame.CurSliceID > frame.SliceCount {
		fmt.Println("======>>>", frame.String(), hex.EncodeToString(data))
		return NCInvalidVideoFrame
	}
	return nil
}

func (tc *H264Conn) readFrame() (*Slice, error) {

	var frame = &VideoFrame{}
	var buf = make([]byte, IceUdpMtu+NinHeaderLen)
	var n, err = tc.connReader.Read(buf)
	//fmt.Println("******>>>readFrame data:", err, hex.EncodeToString(Buf[:n]))
	if err != nil || n < NinHeaderLen {
		return nil, fmt.Errorf("slice header err: %v-%d", err, n)
	}

	err = ParseFrame(frame, buf[:NinHeaderLen])
	if err != nil {
		return nil, err
	}

	buf = buf[NinHeaderLen:n]
	var sliceLen = len(buf)
	if sliceLen != int(frame.CurLen) {
		fmt.Println("******>>>", sliceLen, frame.String(), hex.EncodeToString(buf))
		return nil, NCOneBadFrameData
	}
	fmt.Println("******>>> tlv got:", frame.String())
	return &Slice{
		frame,
		buf,
	}, nil
}
func (tc *H264Conn) parseSlice(buffer chan []byte) {
	for {
		var slice, ok = <-tc.sliceCache
		if !ok {
			fmt.Println("======>>>slice parsing quit")
			return
		}

		var curIdx = slice.Header.FrameID % FrameStackSize
		var curFrame = tc.frameStack[curIdx]
		if curFrame == nil || curFrame.FrameID != slice.Header.FrameID {
			fmt.Println("******>>> create new receive frame for slice:",
				curFrame == nil, slice.Header.String())
			curFrame = &ReceiveFrame{
				FrameID:     slice.Header.FrameID,
				SliceHasGot: 0,
				HasFinished: false,
				Cache:       make([]*Slice, slice.Header.SliceCount),
			}
			tc.frameStack[curIdx] = curFrame
		}

		if curFrame.Cache[slice.Header.CurSliceID] != nil {
			fmt.Println("******>>> resend slice:",
				curFrame.String(), slice.Header.String())
			continue
		}

		curFrame.Cache[slice.Header.CurSliceID] = slice
		curFrame.SliceHasGot = curFrame.SliceHasGot + 1

		if curFrame.SliceHasGot < slice.Header.SliceCount {
			fmt.Println("******>>> read more slice:",
				curFrame.String(), slice.Header.String())
			continue
		}

		curFrame.HasFinished = true
		fmt.Println("******>>>one frame finished:", curIdx, slice.Header.String(), curFrame.String())
		buffer <- curFrame.Flush()
		tc.frameStack[curIdx] = nil
	}
}

func (tc *H264Conn) LoopRead(buffer chan []byte) error {
	tc.sliceCache = make(chan *Slice, 1<<10)

	go tc.parseSlice(buffer)

	for {
		var slice, err = tc.readFrame()
		if err != nil {
			close(tc.sliceCache)
			return err
		}
		tc.sliceCache <- slice
	}
}

func (tc *H264Conn) WriteVideoFrame(buf []byte) (n int, err error) {
	tc.frameCounter++
	var dataLen = len(buf)
	//fmt.Println("======>>> tlv need to write ", dataLen, hex.EncodeToString(Buf))

	var sliceSize = dataLen / IceUdpMtu
	if dataLen%IceUdpMtu > 0 {
		sliceSize = sliceSize + 1
	}
	var frame = &VideoFrame{
		FrameID:    tc.frameCounter,
		SliceCount: uint16(sliceSize),
	}

	var sequence uint16 = 0
	for startIdx := 0; startIdx < dataLen; startIdx = startIdx + IceUdpMtu {
		var endIdx = startIdx + IceUdpMtu
		var sliceLen = IceUdpMtu
		if endIdx > dataLen {
			endIdx = dataLen
			sliceLen = dataLen - startIdx
		}
		frame.CurSliceID = sequence
		frame.CurLen = uint16(sliceLen)

		var dataToWrite = frame.Data()
		dataToWrite = append(dataToWrite, buf[startIdx:endIdx]...)

		n, err = tc.connWriter.Write(dataToWrite)
		if err != nil || n == 0 {
			return
		}
		fmt.Println("======>>> tlv write ", n, frame.String()) //, hex.EncodeToString(dataToWrite))
		sequence = sequence + 1
	}

	return dataLen, nil
}

func FrameWrite(source func() ([]byte, error), conn io.Writer) error {
	var writer = &H264Conn{connWriter: conn}
	for {
		var data, err = source()
		if err != nil {
			return err
		}
		var _, errW = writer.WriteVideoFrame(data)
		if errW != nil {
			return errW
		}
	}
}
