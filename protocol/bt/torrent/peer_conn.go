package torrent

import (
	"bufio"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/RoaringBitmap/roaring"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/cenkalti/mse"
	"github.com/monkeyWie/gopeed/protocol/bt/metainfo"
	"github.com/monkeyWie/gopeed/protocol/bt/peer"
	"github.com/monkeyWie/gopeed/protocol/bt/peer/message"
	log "github.com/sirupsen/logrus"
)

// 现在主流客户端的block大小都是16KB
const blockSize = 2 << 13
const keepaliveTimeout = 60 * 2

var errPieceCheckFailed = errors.New("piece check failed")
var keepaliveData = make([]byte, 4)

type peerConn struct {
	torrent *Torrent
	peer    *peer.Peer
	conn    net.Conn
	// this client is choking the peer
	amChoking bool
	// this client is interested in the peer
	amInterested bool
	// peer is choking this client
	peerChoking bool
	// peer is interested in this client
	peerInterested bool

	// 上一次收到包的时间
	lastReciveTime int64

	bitfield *message.Bitfield
	readyEnd bool

	downloadedCh chan error
	disconnectCh chan error
	// block下载队列，官方推荐为5
	blockQueueCh chan interface{}
}

func newPeerConn(torrent *Torrent, peer *peer.Peer) *peerConn {
	return &peerConn{
		torrent: torrent,
		peer:    peer,
	}
}

// 使用MSE加密来避免运营商对bt流量的封锁，基本上现在市面上BT客户端都默认开启了，不用MSE的话很多Peer拒绝连接
// see http://wiki.vuze.com/w/Message_Stream_Encryption
func (pc *peerConn) dialMse() error {
	conn, err := net.DialTimeout("tcp", pc.peer.Address(), time.Second*time.Duration(30))
	if err != nil {
		return err
	}
	mseConn := mse.WrapConn(conn)
	infoHash := pc.torrent.MetaInfo.GetInfoHash()
	_, err = mseConn.HandshakeOutgoing(infoHash[:], mse.PlainText, nil)
	if err != nil {
		mseConn.Close()
		return err
	}
	pc.conn = mseConn
	return nil
}

// Handshake of Peer wire protocol
// see https://wiki.theory.org/index.php/BitTorrentSpecification#Handshake
func (pc *peerConn) handshake() (*peer.Handshake, error) {
	handshakeRes, err := func() (*peer.Handshake, error) {
		handshakeReq := peer.NewHandshake([8]byte{}, pc.torrent.MetaInfo.GetInfoHash(), pc.torrent.PeerID)
		_, err := pc.conn.Write(handshakeReq.Encode())
		if err != nil {
			return nil, err
		}
		var read [68]byte
		_, err = io.ReadFull(pc.conn, read[:])
		if err != nil {
			return nil, err
		}
		handshakeRes := &peer.Handshake{}
		err = handshakeRes.Decode(read[:])
		if err != nil {
			return nil, err
		}
		// InfoHash不匹配
		if handshakeRes.InfoHash != handshakeReq.InfoHash {
			return nil, fmt.Errorf("info_hash not currently serving")
		}
		return handshakeRes, nil
	}()
	if err != nil {
		pc.conn.Close()
		return nil, err
	}
	// init state
	pc.amChoking = true
	pc.amInterested = false
	pc.peerChoking = true
	pc.peerInterested = false
	return handshakeRes, nil
}

func (pc *peerConn) handleKeepalive() {
	pc.lastReciveTime = time.Now().Unix()
	for {
		time.Sleep(time.Minute * 2)
		// 如果超过两分钟没有响应，则断开连接
		if time.Now().Unix()-pc.lastReciveTime > keepaliveTimeout {
			pc.conn.Close()
			break
		}
		// 两分钟发送一次心跳包
		_, err := pc.conn.Write(keepaliveData)
		if err != nil {
			pc.conn.Close()
			break
		}
	}
}

// 准备下载
func (pc *peerConn) ready() error {
	if err := pc.dialMse(); err != nil {
		return fmt.Errorf("tcp dial error %w", err)
	}
	if _, err := pc.handshake(); err != nil {
		return fmt.Errorf("handshake error %w", err)
	}
	pc.readyEnd = false
	readyCh := make(chan bool)
	pc.disconnectCh = make(chan error)
	go pc.handleKeepalive()
	go func() {
		scanner := bufio.NewScanner(pc.conn)
		scanner.Split(message.SplitMessage)
		for scanner.Scan() {
			pc.lastReciveTime = time.Now().Unix()
			buf := scanner.Bytes()
			length := binary.BigEndian.Uint32(buf[:4])
			if length == 0 {
				// 	keepalive
			} else {
				switch message.ID(buf[4]) {
				case message.IdChoke:
					break
				case message.IdUnchoke:
					pc.handleUnchoke(readyCh)
					break
				case message.IdInterested:
					break
				case message.IdNotInterested:
					break
				case message.IdHave:
					break
				case message.IdBitfield:
					pc.handleBitfield(buf)
					break
				case message.IdRequest:
					break
				case message.IdPiece:
					pc.handlePiece(buf)
					break
				case message.IdCancel:
					break
				}
			}
		}
		// 还未下载完成时连接断开
		err := scanner.Err()
		if err == nil {
			err = io.EOF
		}
		pc.disconnectCh <- err
		close(pc.disconnectCh)
	}()
	err := func() error {
		select {
		case status := <-readyCh:
			if status {
				return nil
			}
			return errors.New("ready fail")
		case <-time.After(time.Second * 30):
			// 30秒之后超时
			return errors.New("ready time out")
		}
	}()
	if err != nil {
		pc.conn.Close()
		return err
	}
	return nil
}

// 下载指定piece
func (pc *peerConn) downloadPiece(index int) (err error) {
	pieceLength := pc.torrent.MetaInfo.GetPieceLength(index)
	pc.downloadedCh = make(chan error)
	pc.blockQueueCh = make(chan interface{}, 5)
	defer close(pc.blockQueueCh)
	// 按块下载分片
	blockCount := pc.torrent.pieceStates.states[index].blockCount
	for i := 0; i < blockCount; i++ {
		offset := blockSize * i
		// 如果已经下载过就跳过
		if pc.torrent.pieceStates.isBlockDone(index, offset) {
			continue
		}

		var blockLength uint32
		if i == blockCount-1 {
			// 最后一个block大小需要计算出来
			blockLength = uint32(pieceLength - offset)
		} else {
			blockLength = blockSize
		}
		// block下载排队
		select {
		case pc.blockQueueCh <- nil:
			break
		case err = <-pc.downloadedCh:
			break
		case err = <-pc.disconnectCh:
			break
		}
		// 如果连接出现问题或下载失败直接返回异常
		if err != nil {
			pc.conn.Close()
			return
		}
		// 发起request，对方会响应piece
		_, err = pc.conn.Write(message.BuildRequest(uint32(index), uint32(offset), blockLength).Encode())
		if err != nil {
			break
		}
	}
	select {
	case err = <-pc.downloadedCh:
		break
	case err = <-pc.disconnectCh:
		break
	}
	if err != nil {
		pc.conn.Close()
	}
	return
}

func (pc *peerConn) handleUnchoke(readyCh chan<- bool) {
	pc.peerChoking = false
	// 已经处理过Unchoke信号
	if pc.readyEnd {
		return
	}
	pc.readyEnd = true
	// 如果客户端对peer感兴趣并且peer没有choked客户端，就可以开始下载了
	if pc.amInterested {
		readyCh <- true
	} else {
		readyCh <- false
	}
	close(readyCh)
}

func (pc *peerConn) handleBitfield(buf []byte) {
	pc.bitfield = message.NewBitfield()
	pc.bitfield.Decode(buf)
	have := pc.getHavePieces(pc.bitfield)
	if len(have) > 0 {
		// 表示对该peer感兴趣，并且不choked该peer
		pc.conn.Write(message.NewInterested().Encode())
		pc.amInterested = true

		pc.conn.Write(message.NewUnchoke().Encode())
		pc.amChoking = false
	} else {
		pc.conn.Close()
	}
}

// 处理下载响应，每次接收到响应直接将block写入到对应文件中
func (pc *peerConn) handlePiece(buf []byte) {
	piece := message.NewPiece()
	piece.Decode(buf)
	info := pc.torrent.MetaInfo.Info
	fds := pc.torrent.MetaInfo.GetFileDetails()
	blockLength := int64(len(piece.Block))
	pieceBegin := int64(piece.Index) * int64(info.PieceLength)
	blockBegin := pieceBegin + int64(piece.Begin)
	// 计算block对应要写到的文件偏移
	var fileBlocks []*fileBlock
	if len(info.Files) == 0 {
		// 单文件
		fileBlocks = append(fileBlocks, &fileBlock{
			filepath:   info.Name,
			fileSeek:   blockBegin,
			blockBegin: 0,
			blockEnd:   blockLength,
		})
	} else {
		// 获取要写入的第一个文件和偏移
		writeIndex := getWriteFile(blockBegin, fds)
		// block对应种子的偏移
		blockFileBegin := blockBegin
		// block对应文件的偏移
		var blockSeek int64 = 0
		for _, f := range fds[writeIndex:] {
			// 计算文件可剩余写入字节数
			fileWritable := f.End - blockFileBegin
			// 计算block剩余写入字节数
			blockWritable := blockLength - blockSeek
			fb := fileBlock{
				filepath:   filepath.Join(f.Path...),
				fileSeek:   blockFileBegin - f.Begin,
				blockBegin: blockSeek,
			}
			if fileWritable >= blockWritable {
				// 若够block写入直接跳出循环
				fb.blockEnd = blockSeek + blockWritable
				fileBlocks = append(fileBlocks, fb)
				break
			} else {
				// 否则计算剩余可写字节数，写入到下一个文件中
				fb.blockEnd = blockSeek + fileWritable
				fileBlocks = append(fileBlocks, fb)
				blockFileBegin = f.End
				blockSeek += fileWritable
			}
		}
	}
	// 开始写入文件
	for _, f := range fileBlocks {
		err := func() error {
			name := filepath.Join(pc.torrent.Path, f.filepath)
			file, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0644)
			if err != nil {
				return err
			}
			defer file.Close()
			_, err = file.WriteAt(piece.Block[f.blockBegin:f.blockEnd], f.fileSeek)
			if err != nil {
				return err
			}
			return nil
		}()
		if err != nil {
			pc.downloadedCh <- errPieceCheckFailed
			pc.conn.Close()
			return
		}
	}
	pc.torrent.pieceStates.setBlockDone(int(piece.Index), int(piece.Begin))
	// 出队
	<-pc.blockQueueCh

	log.Debugf("piece:%d block:%d", piece.Index, piece.Begin)

	// piece全部下载完
	if pc.torrent.pieceStates.isPieceDone(int(piece.Index)) {
		// 计算piece对应的文件偏移
		sha1 := sha1.New()
		pieceLength := pc.torrent.MetaInfo.GetPieceLength(int(piece.Index))
		writeIndex := getWriteFile(pieceBegin, fds)
		fileBegin := pieceBegin - fds[writeIndex].Begin
		for _, fd := range fds[writeIndex:] {
			func() {
				file, err := os.Open(path.Join(pc.torrent.Path, fd.Path[0]))
				if err != nil {
					panic(err)
				}
				defer file.Close()
				_, err = file.Seek(fileBegin, 0)
				if err != nil {
					panic(err)
				}
				written, err := io.CopyN(sha1, file, int64(pieceLength))
				if err != nil {
					if err != io.EOF {
						panic(err)
					}
				}
				pieceLength -= int(written)
			}()
			if pieceLength > 0 {
				// 	继续读下个文件
				fileBegin = 0
			} else {
				break
			}
		}
		// 断开连接 TODO 用连接池进行复用
		pc.conn.Close()

		// 校验piece SHA-1 hash
		downHash := [20]byte{}
		copy(downHash[:], sha1.Sum(nil))
		if downHash == pc.torrent.MetaInfo.Info.Pieces[piece.Index] {
			// piece下载完成
			pc.downloadedCh <- nil
		} else {
			pc.downloadedCh <- errPieceCheckFailed
		}
		close(pc.downloadedCh)
	}
}

// 获取piece对应要写入的文件
func getWriteFile(pieceBegin int64, fds []*metainfo.FileDetail) int {
	for i, f := range fds {
		if f.Begin <= pieceBegin && f.End > pieceBegin {
			return i
		}
	}
	return -1
}

type fileBlock struct {
	filepath   string
	fileSeek   int64
	blockBegin int64
	blockEnd   int64
}

// 获取peer能提供需要下载的文件分片
func (pc *peerConn) getHavePieces(bitfield *message.Bitfield) []uint32 {
	had := roaring.New()
	for i := 0; i < pc.torrent.pieceStates.size(); i++ {
		if pc.torrent.pieceStates.getState(i) == stateFinish {
			had.AddInt(i)
		}
	}
	return bitfield.Provide(had)
}

// 获取要写入到的文件
/*func (ps *peerConn) getWriteFile(request *message.Request) string {
	info := ps.torrent.MetaInfo.Info
	// 单文件
	if len(info.Files) == 0 {
		return info.Name
	} else {
		request.Index * info.PieceLength + be
		for i := 0; i < len(info.Files); i++ {

		}
	}
}
*/
