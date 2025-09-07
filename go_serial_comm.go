package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"hash/crc32"

	"github.com/klauspost/reedsolomon"
	"github.com/tarm/serial"
)

// 添加尾部同步字常量
const (
	SYNC_PATTERN              = 0xA5A5A5A5         // 同步模式
	SYNC_HEADER               = "\xA5\xA5\xA5\xA5" // 头部同步字
	SYNC_TAIL                 = 0x0F0F0F0F         // 尾部同步字
	SYNC_TAIL_STR             = "\x0F\x0F\x0F\x0F" // 尾部同步字字符串
	PACKET_TYPE_DATA          = 0x01               // 數據包類型
	PACKET_TYPE_ACK           = 0x02               // 確認包類型
	PACKET_ACK_SIZE           = 27                 // 確認包大小(不含尾部同步字)
	PACKET_ACK_SIZE_WITH_TAIL = 32                 // 确认包大小(含尾部同步字)
	PACKET_DATA_SIZE          = 23
	SP_Head                   = 0x5350735053707370 // 'SP' 包头标识 'SPsPSpsp'
	SP_Head_STR               = "SPsPSpsp"
)

// ToHex returns the hex representation of the byte slice as a string.
func ToHex(data []byte) string {
	var sb strings.Builder

	for i, b := range data {
		sb.WriteString(fmt.Sprintf("%02X ", b))

		// 每行显示16个字节
		if (i+1)%32 == 0 {
			sb.WriteString("\n")
		}
	}

	// 去除最后可能多余的空格或换行（可选）
	result := sb.String()
	if len(result) > 0 && result[len(result)-1] == ' ' {
		result = result[:len(result)-1]
	}

	sb.WriteString(" ")

	return result
}

// 數據包結構
type Packet struct {
	// SyncHeader   uint32 // 同步頭 0xA5A5A5A5
	SyncHeader   []byte
	PacketType   uint8  // 包類型
	SeqNum       uint32 // 序列號
	DataLen      uint16 // 數據長度
	ParityLen    uint16 // 校驗數據長度
	DataShards   uint8  // Reed-Solomon數據分片數
	ParityShards uint8  // Reed-Solomon校驗分片數
	Data         []byte // 原始數據
	Parity       []byte // Reed-Solomon校驗數據
	Checksum     uint32 // 校驗和 (不包括同步頭)
	SyncTail     []byte
	// SyncTail     uint32 // 尾部同步字 0x0F0F0F0F
}

// 序列化數據包
func (p *Packet) Serialize(debug int) []byte {
	fakeData := make([]byte, len(p.Data))
	copy(fakeData, p.Data)
	// 測試用例，加入隨機干擾錯誤
	if debug > 0 {
		if rand.Float32() < 0.3 {
			d_pos := rand.Intn(len(fakeData))
			// 使用异或操作翻转一个随机位，而不是简单地加减1
			// 这样可以确保即使原值是0或255也能正确处理
			fakeData[d_pos] ^= 1 << uint(rand.Intn(8))
		}
	}

	buf := new(bytes.Buffer)
	// binary.Write(buf, binary.LittleEndian, p.SyncHeader)
	buf.Write(p.SyncHeader)
	binary.Write(buf, binary.LittleEndian, p.PacketType)
	binary.Write(buf, binary.LittleEndian, p.SeqNum)
	binary.Write(buf, binary.LittleEndian, p.DataLen)
	binary.Write(buf, binary.LittleEndian, p.ParityLen)
	binary.Write(buf, binary.LittleEndian, p.DataShards)
	binary.Write(buf, binary.LittleEndian, p.ParityShards)
	// buf.Write(p.Data)
	buf.Write(fakeData)
	buf.Write(p.Parity)
	binary.Write(buf, binary.LittleEndian, p.Checksum)
	// binary.Write(buf, binary.LittleEndian, p.SyncTail) // 添加尾部同步字
	buf.Write(p.SyncTail)
	return buf.Bytes()
}

// 反序列化數據包
func DeserializePacket(data []byte) (*Packet, error) {
	if len(data) < PACKET_DATA_SIZE { // 最小包大小: 4+1+4+2+2+1+1+4+4 = 23
		return nil, fmt.Errorf("packet too small: %d bytes", len(data))
	}

	buf := bytes.NewReader(data)
	p := &Packet{
		SyncHeader: make([]byte, 4),
		SyncTail:   make([]byte, 4),
	}
	// binary.Read(buf, binary.LittleEndian, &p.SyncHeader)
	// if p.SyncHeader != SYNC_PATTERN {
	// 	return nil, fmt.Errorf("invalid sync pattern: 0x%X", p.SyncHeader)
	// }
	buf.Read(p.SyncHeader)
	if !bytes.Equal(p.SyncHeader, []byte(SYNC_HEADER)) {
		return nil, fmt.Errorf("invalid sync header: %s", ToHex(p.SyncHeader))
	}

	binary.Read(buf, binary.LittleEndian, &p.PacketType)
	if p.PacketType != PACKET_TYPE_DATA {
		return nil, fmt.Errorf("invalid packet type: %d", p.PacketType)
	}

	binary.Read(buf, binary.LittleEndian, &p.SeqNum)
	binary.Read(buf, binary.LittleEndian, &p.DataLen)
	binary.Read(buf, binary.LittleEndian, &p.ParityLen)
	binary.Read(buf, binary.LittleEndian, &p.DataShards)
	binary.Read(buf, binary.LittleEndian, &p.ParityShards)

	// 檢查數據長度是否合理
	expectedLen := int(p.DataLen + p.ParityLen + PACKET_DATA_SIZE)
	if len(data) != expectedLen {
		return nil, fmt.Errorf("packet length mismatch: expected %d, got %d, dataLen %d, parityLen %d", expectedLen, len(data), p.DataLen, p.ParityLen)
	}

	p.Data = make([]byte, p.DataLen)
	buf.Read(p.Data)

	p.Parity = make([]byte, p.ParityLen)
	buf.Read(p.Parity)

	binary.Read(buf, binary.LittleEndian, &p.Checksum)

	// 读取尾部同步字
	// binary.Read(buf, binary.LittleEndian, &p.SyncTail)
	// if p.SyncTail != SYNC_TAIL {
	// 	return nil, fmt.Errorf("invalid sync tail: 0x%X", p.SyncTail)
	// }
	buf.Read(p.SyncTail)
	if !bytes.Equal(p.SyncTail, []byte(SYNC_TAIL_STR)) {
		return nil, fmt.Errorf("invalid sync tail: %s", ToHex(p.SyncTail))
	}

	return p, nil
}

type SP_Packet struct {
	Header    []byte // 包頭標識 'SP' 包头标识 'SPsPSpsp'
	SP_Seq    uint16 // SP組序列號，同一組的SP包的序列號應該是一樣的
	SP_Length uint16 // 分塊數量
	SP_seqnum uint16 // 分塊序列號
	DataLen   uint16 // 分塊數據長度
	SP_Data   []byte // 分塊數據
}

func New_SP_Packet(data []byte, sp_size int, seq uint16) []*SP_Packet {
	var packets []*SP_Packet
	for i := 0; i < len(data); i += sp_size {
		end := min(i+sp_size, len(data))
		packet := &SP_Packet{
			Header:    []byte(SP_Head_STR), // 'SP' 的十六進制表示
			SP_Seq:    seq,
			SP_Length: uint16((len(data) + sp_size - 1) / sp_size), // 分塊數量
			SP_seqnum: uint16(i / sp_size),
			DataLen:   uint16(end - i),
			SP_Data:   data[i:end],
		}
		packets = append(packets, packet)
	}
	return packets
}

// 合併分片包，檢查輸入數據的合理性，若都是同一個SP_Seq，
// 滿足SP_Length，又沒有重複的，則按SP_seqnum的順序排序進行合并
func Concat_SP_Packets(packets []*SP_Packet) []byte {
	var buf bytes.Buffer
	// 檢查分片數量是否匹配
	if len(packets) != int(packets[0].SP_Length) {
		return nil
	}
	// 下面檢測 SP_seqnum 有無重複的，若有則返回nil
	seqnumMap := make(map[uint16]struct{})
	for _, p := range packets {
		if p.SP_Seq != packets[0].SP_Seq {
			return nil
		}
		if _, exists := seqnumMap[p.SP_seqnum]; exists {
			return nil
		}
		seqnumMap[p.SP_seqnum] = struct{}{}
	}
	// 進行合并
	sort.Slice(packets, func(i, j int) bool {
		return packets[i].SP_seqnum < packets[j].SP_seqnum
	})
	for _, p := range packets {
		buf.Write(p.SP_Data)
	}
	return buf.Bytes()
}

func (sp *SP_Packet) Serialize() []byte {
	buf := new(bytes.Buffer)
	buf.Write(sp.Header)
	binary.Write(buf, binary.LittleEndian, sp.SP_Seq)
	binary.Write(buf, binary.LittleEndian, sp.SP_Length)
	binary.Write(buf, binary.LittleEndian, sp.SP_seqnum)
	binary.Write(buf, binary.LittleEndian, sp.DataLen)
	buf.Write(sp.SP_Data)
	return buf.Bytes()
}

func Deserialize_SP_Packet(data []byte) (*SP_Packet, error) {
	if len(data) < 16 {
		return nil, fmt.Errorf("invalid SP packet size: %d", len(data))
	}

	buf := bytes.NewReader(data)
	p := &SP_Packet{
		Header: make([]byte, 8),
	}

	buf.Read(p.Header)
	if !bytes.Equal(p.Header, []byte(SP_Head_STR)) {
		return nil, fmt.Errorf("invalid SP header: %s ", ToHex(p.Header))
	}
	binary.Read(buf, binary.LittleEndian, &p.SP_Seq)
	binary.Read(buf, binary.LittleEndian, &p.SP_Length)
	binary.Read(buf, binary.LittleEndian, &p.SP_seqnum)
	binary.Read(buf, binary.LittleEndian, &p.DataLen)

	if int(p.DataLen)+16 != len(data) {
		return nil, fmt.Errorf("invalid SP data length: expected %d, got %d", p.DataLen+16, len(data))
	}
	p.SP_Data = make([]byte, int(p.DataLen))
	buf.Read(p.SP_Data)

	return p, nil
}

// AckPacket 確認包結構
type AckPacket struct {
	SyncHeader uint32 // 同步頭 0xA5A5A5A5
	PacketType uint8  // 包類型 (PACKET_TYPE_ACK)
	BaseSeq    uint32 // 基準序列號
	// ReceiveLen：Bitmap中的接收長度，即已經接收過的data packet數量在Bitmap中的體現
	// （不論是否已通過校驗，準確接收）這個值要相對與 BaseSeq 計算
	ReceiveLen   uint8
	Bitmap       uint64 // 狀態位圖 (64個包的狀態)
	DataShards   uint8  // Reed-Solomon數據分片數
	ParityShards uint8  // Reed-Solomon校驗分片數
	AckSeqNum    uint32 // ACK包序列號
	Checksum     uint32 // 校驗和 (不包括同步頭)
	SyncTail     uint32 // 尾部同步字 0x5A5A5A5A
}

// 序列化確認包
func (a *AckPacket) Serialize() []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, a.SyncHeader)
	binary.Write(buf, binary.LittleEndian, a.PacketType)
	binary.Write(buf, binary.LittleEndian, a.BaseSeq)
	binary.Write(buf, binary.LittleEndian, a.ReceiveLen)
	binary.Write(buf, binary.LittleEndian, a.Bitmap)
	binary.Write(buf, binary.LittleEndian, a.DataShards)   // 新增字段
	binary.Write(buf, binary.LittleEndian, a.ParityShards) // 新增字段
	binary.Write(buf, binary.LittleEndian, a.AckSeqNum)    // ACK序列號
	binary.Write(buf, binary.LittleEndian, a.Checksum)
	binary.Write(buf, binary.LittleEndian, a.SyncTail) // 添加尾部同步字
	return buf.Bytes()
}

// 反序列化確認包
func DeserializeAckPacket(portName string, data []byte) (*AckPacket, error) {
	// 確認包大小應該是 31 字節: 4+1+4+1+8+1+1+4+4+4 = 32
	if len(data) != PACKET_ACK_SIZE_WITH_TAIL {
		return nil, fmt.Errorf("%s ack packet size mismatch: expected %d, got %d", portName, PACKET_ACK_SIZE_WITH_TAIL, len(data))
	}

	buf := bytes.NewReader(data)
	a := &AckPacket{}

	// 按照序列化順序讀取各個字段
	err := binary.Read(buf, binary.LittleEndian, &a.SyncHeader)
	if err != nil {
		return nil, fmt.Errorf("failed to read SyncHeader: %v", err)
	}

	if a.SyncHeader != SYNC_PATTERN {
		return nil, fmt.Errorf("invalid sync pattern in ack: 0x%X", a.SyncHeader)
	}

	err = binary.Read(buf, binary.LittleEndian, &a.PacketType)
	if err != nil {
		return nil, fmt.Errorf("failed to read PacketType: %v", err)
	}

	if a.PacketType != PACKET_TYPE_ACK {
		return nil, fmt.Errorf("invalid ack packet type: %d", a.PacketType)
	}

	err = binary.Read(buf, binary.LittleEndian, &a.BaseSeq)
	if err != nil {
		return nil, fmt.Errorf("failed to read BaseSeq: %v", err)
	}

	err = binary.Read(buf, binary.LittleEndian, &a.ReceiveLen)
	if err != nil {
		return nil, fmt.Errorf("failed to read ReceiveLen: %v", err)
	}

	err = binary.Read(buf, binary.LittleEndian, &a.Bitmap)
	if err != nil {
		return nil, fmt.Errorf("failed to read Bitmap: %v", err)
	}

	err = binary.Read(buf, binary.LittleEndian, &a.DataShards)
	if err != nil {
		return nil, fmt.Errorf("failed to read DataShards: %v", err)
	}

	err = binary.Read(buf, binary.LittleEndian, &a.ParityShards)
	if err != nil {
		return nil, fmt.Errorf("failed to read ParityShards: %v", err)
	}

	err = binary.Read(buf, binary.LittleEndian, &a.AckSeqNum)
	if err != nil {
		return nil, fmt.Errorf("failed to read AckSeqNum: %v", err)
	}

	err = binary.Read(buf, binary.LittleEndian, &a.Checksum)
	if err != nil {
		return nil, fmt.Errorf("failed to read Checksum: %v", err)
	}

	// 读取尾部同步字
	err = binary.Read(buf, binary.LittleEndian, &a.SyncTail)
	if err != nil {
		return nil, fmt.Errorf("failed to read SyncTail: %v", err)
	}

	if a.SyncTail != SYNC_TAIL {
		return nil, fmt.Errorf("invalid sync tail in ack: 0x%X", a.SyncTail)
	}

	return a, nil
}

// 接收緩衝區管理
type ReceiveBuffer struct {
	buffer    []byte // 原始接收緩衝區
	syncState int    // 同步狀態
	// expectLen  int    // 期望的數據包長度
	// currentLen int    // 當前已接收長度
}

// type sendRawChanPacket struct {
// 	timer int64
// 	data  []byte
// }

// 滑動窗口通信器
// 滑動窗口通信器
type SerialComm struct {
	port                  *serial.Port
	portName              string // 添加端口名稱字段
	windowSize            int
	sendBuffer            map[uint32]*Packet  // 發送緩衝區
	recvBuffer            map[uint32]*Packet  // 接收緩衝區
	recvWindow            map[uint32]*Packet  // 接收窗口
	sendBase              uint32              // 發送窗口基準
	sendNext              uint32              // 下一個發送序列號
	recvBase              uint32              // 接收窗口基準
	recvMax               uint32              // 接收窗口最大值
	firstRecvBool         bool                // 標識是否是第一次接受，用來初始化recvBase
	dataShards            int                 // Reed-Solomon數據分片數
	parityShards          int                 // Reed-Solomon校驗分片數
	rsEncoder             reedsolomon.Encoder // Reed-Solomon編碼器
	errorRate             float32             // 當前誤碼率
	receiveBuffer         *ReceiveBuffer      // 接收緩衝區管理
	recvChan              chan []byte
	mutex                 sync.Mutex
	ackTimer              *time.Timer
	retransmitTimer       *time.Timer
	done                  chan bool
	BaudRate              int
	isRunning             bool
	ACK_Count             int
	ACKChan               chan []byte
	DataChan              chan []byte
	RetransmitChan        chan []byte
	EndTime               int64
	debug                 int
	ackSeqNum             uint32 // ACK包序列號
	sp_packet             map[uint16][]*SP_Packet
	sp_seq                uint16
	ackLoop_reset_Chan    chan struct{}
	circleSend_reset_Chan chan struct{}
}

// 創建串口通信器
func NewSerialComm(portName string, baud int, debug int) (*SerialComm, error) {
	config := &serial.Config{
		Name: portName,
		Baud: baud,
	}

	port, err := serial.OpenPort(config)
	if err != nil {
		return nil, err
	}

	// 初始化Reed-Solomon編碼器 (16個數據分片 + 4個校驗分片)
	dataShards := 16
	parityShards := 4
	rsEncoder, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		port.Close()
		return nil, fmt.Errorf("failed to create Reed-Solomon encoder: %v", err)
	}

	sc := &SerialComm{
		port:          port,
		portName:      portName, // 記錄端口名稱
		BaudRate:      baud,
		windowSize:    16, // 初始窗口大小
		sendBuffer:    make(map[uint32]*Packet),
		recvBuffer:    make(map[uint32]*Packet),
		recvWindow:    make(map[uint32]*Packet),
		firstRecvBool: true,
		dataShards:    dataShards,
		parityShards:  parityShards,
		rsEncoder:     rsEncoder,
		errorRate:     0.0,
		receiveBuffer: &ReceiveBuffer{
			buffer:    make([]byte, 0, 4096),
			syncState: 0,
		},
		isRunning:             true,
		recvChan:              make(chan []byte, 16),
		ACK_Count:             0,
		ACKChan:               make(chan []byte),
		DataChan:              make(chan []byte),
		RetransmitChan:        make(chan []byte),
		sp_packet:             make(map[uint16][]*SP_Packet),
		done:                  make(chan bool),
		EndTime:               0,
		debug:                 debug, // 調試級別
		sp_seq:                0,
		ackLoop_reset_Chan:    make(chan struct{}),
		circleSend_reset_Chan: make(chan struct{}),
	}

	if sc.debug > 0 {
		log.Printf("port %s Created SerialComm instance ", portName)
	}

	// 啟動接收協程
	go sc.receiveLoop()
	// 啟動確認發送協程
	go sc.ackLoop()
	// 沒有收到任何ack包，要警覺了，開始循環發送 sendWindow數據
	go sc.circleSendLoop()
	// 確認發送包的發送改成由 updateReciveLoop 啓動，
	// 實現 sc.WindowSize /4 發一次
	// 定時器只是輔助作用
	// 即 正確收到 4個包後，才發送一次確認包
	// 1. 當然這裏有個假設，就是 發出來64個包，至少要正確收到 4個包，否則bitmap就爆了

	// 啓動發送協程
	// go sc.SendRowAPI()

	return sc, nil
}

// 關閉通信器
func (sc *SerialComm) Close() {
	close(sc.done)
	sc.isRunning = false
	if sc.retransmitTimer != nil {
		sc.retransmitTimer.Stop()
	}
	if sc.ackTimer != nil {
		sc.ackTimer.Stop()
	}
	if sc.debug > 0 {
		log.Printf("port %s Closed.", sc.portName)
	}
	sc.port.Close()
}

// Reed-Solomon FEC編碼
func (sc *SerialComm) encodeFEC(data []byte) ([][]byte, error) {
	// 計算每個分片的大小
	shardSize := (len(data) + sc.dataShards - 1) / sc.dataShards
	totalSize := shardSize * sc.dataShards

	// 填充數據到總大小
	paddedData := make([]byte, totalSize)
	copy(paddedData, data)

	// 創建分片
	shards := make([][]byte, sc.dataShards+sc.parityShards)

	// 填充數據分片
	for i := 0; i < sc.dataShards; i++ {
		shards[i] = paddedData[i*shardSize : (i+1)*shardSize]
	}

	// 創建校驗分片
	for i := sc.dataShards; i < sc.dataShards+sc.parityShards; i++ {
		shards[i] = make([]byte, shardSize)
	}

	// 編碼
	err := sc.rsEncoder.Encode(shards)
	return shards, err
}

// Reed-Solomon FEC解碼
func (sc *SerialComm) decodeFECWithEncoder(encoder reedsolomon.Encoder, shards [][]byte, dataLen int, dataShards int) ([]byte, error) {
	// 驗證並嘗試修復
	ok, err := encoder.Verify(shards)
	if err != nil {
		return nil, err
	}

	if !ok {
		// 數據有錯誤，嘗試重構
		err = encoder.Reconstruct(shards)
		if err != nil {
			return nil, fmt.Errorf("failed to reconstruct data: %v", err)
		}
	}

	// 重新組合數據
	result := make([]byte, 0, dataLen)
	for i := 0; i < dataShards; i++ {
		result = append(result, shards[i]...)
	}

	// 裁剪到原始數據長度
	if len(result) > dataLen {
		result = result[:dataLen]
	}

	return result, nil
}

// 計算校驗和
func calculateChecksum(data []byte) uint32 {
	crc32Hash := crc32.NewIEEE()
	crc32Hash.Write(data)
	return crc32Hash.Sum32()
}

func (sc *SerialComm) Ser_Write(src string, data []byte, drop bool) {
	Now := time.Now().UnixNano()
	if drop {
		// 當前沒有發送包時，才會發送（EndTime 是模擬計算的發送延時）
		if Now > sc.EndTime {
			sc.port.Write(data)
			sc.EndTime = Now + int64(1000000*len(data)*9/sc.BaudRate) // 停止位
			if sc.debug > 3 {
				log.Printf("port %s Ser_Write %s drop send %d bytes\n", sc.portName, src, len(data))
			}
		} else {
			if sc.debug > 3 {
				log.Printf("port %s Ser_Write %s drop drop %d bytes\n", sc.portName, src, len(data))
			}
		}
	} else {
		sc.port.Write(data)
		sc.EndTime = sc.EndTime + int64(1000000*len(data)*9/sc.BaudRate) // 停止位
		if sc.debug > 3 {
			log.Printf("port %s Ser_Write %s nodrop send %d bytes\n", sc.portName, src, len(data))
		}
	}
}

// 發送數據準備
func (sc *SerialComm) send_prepare(data []byte) []byte {
	sc.mutex.Lock()
	defer sc.mutex.Unlock()

	if sc.debug > 0 {
		log.Printf("port %s Send calling sendnext %d sendBase %d \n", sc.portName, sc.sendNext, sc.sendBase)
	}
	// 等待窗口有空間
	for sc.sendNext-sc.sendBase >= uint32(sc.windowSize) {
		sc.mutex.Unlock()
		time.Sleep(10 * time.Millisecond)
		sc.mutex.Lock()
	}

	// 使用Reed-Solomon編碼
	shards, err := sc.encodeFEC(data)
	if err != nil {
		log.Printf("port %s FEC encoding failed: %v", sc.portName, err)
		return nil
	}

	// 組合校驗數據
	var parityData []byte
	for i := sc.dataShards; i < sc.dataShards+sc.parityShards; i++ {
		parityData = append(parityData, shards[i]...)
	}

	// 創建數據包
	packet := &Packet{
		// SyncHeader:   SYNC_PATTERN,
		SyncHeader:   []byte(SYNC_HEADER),
		PacketType:   PACKET_TYPE_DATA,
		SeqNum:       sc.sendNext,
		DataLen:      uint16(len(data)),
		ParityLen:    uint16(len(parityData)),
		DataShards:   uint8(sc.dataShards),
		ParityShards: uint8(sc.parityShards),
		Data:         data,
		Parity:       parityData,
		SyncTail:     []byte(SYNC_TAIL_STR),
		// SyncTail:     SYNC_TAIL,
	}

	// 計算校驗和 (不包括同步頭和校驗和)
	// serialized := packet.Serialize()
	// packet.Checksum = calculateChecksum(serialized[4 : len(serialized)-8]) // 跳過同步頭和校驗和
	packet.Checksum = calculateChecksum(data) // 跳過同步頭和校驗和

	// 實際發送
	finalData := packet.Serialize(sc.debug)

	// 存儲到發送緩衝區
	sc.sendBuffer[sc.sendNext] = packet
	sc.sendNext++

	if sc.debug > 2 {
		log.Printf("port %s Send packet seq: %d", sc.portName, packet.SeqNum)
	}
	return finalData
}

// 發送數據
func (sc *SerialComm) Send(data []byte) {
	finalData := sc.send_prepare(data)
	sc.Ser_Write("Data", finalData, false)
}

// 發送數據的同步方式，即數據必須發送完成后才返回
func (sc *SerialComm) Send_sync(data []byte) {
	finalData := sc.send_prepare(data)
	sc.Ser_Write("Data", finalData, false)

	// 更准确的计算：每个字节包括1个起始位+8个数据位+1个停止位=10位
	bitsPerByte := 10
	duration := float64(len(finalData)*bitsPerByte) / float64(sc.BaudRate)

	// 添加少量余量以确保数据完全发送
	duration *= 1.1 // 10%的安全余量

	time.Sleep(time.Duration(duration * float64(time.Second)))
}

func (sc *SerialComm) gzip_data(data []byte) []byte {
	// 使用gzip压缩数据
	var compressedData bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressedData)

	_, err := gzipWriter.Write(data)
	if err != nil {
		log.Printf("port %s Failed to write data to gzip writer: %v", sc.portName, err)
		// 如果压缩失败，使用原始数据
		compressedData.Write(data)
	}

	err = gzipWriter.Close()
	if err != nil {
		log.Printf("port %s Failed to close gzip writer: %v", sc.portName, err)
		// 如果关闭失败，仍然使用已写入的数据
	}
	return compressedData.Bytes()
}
func (sc *SerialComm) ungzip_data(data []byte) []byte {
	// 创建gzip reader来解压数据
	gzipReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		log.Printf("port %s Failed to create gzip reader: %v", sc.portName, err)
		// 如果解压失败，返回原始数据
		return data
	}
	defer gzipReader.Close()

	// 读取解压后的数据
	var decompressedData bytes.Buffer
	_, err = io.Copy(&decompressedData, gzipReader)
	if err != nil {
		log.Printf("port %s Failed to decompress data: %v", sc.portName, err)
		// 如果解压失败，返回原始数据
		return data
	}

	return decompressedData.Bytes()
}
func (sc *SerialComm) Send_Data(data []byte) {
	sp_data := New_SP_Packet(sc.gzip_data(data), 2*1024, sc.sp_seq)
	sc.sp_seq++
	for _, sp := range sp_data {
		sc.Send(sp.Serialize())
	}
}

func (sc *SerialComm) Send_Data_sync(data []byte) {
	sp_data := New_SP_Packet(sc.gzip_data(data), 1024, sc.sp_seq)
	sc.sp_seq++
	for _, sp := range sp_data {
		sc.Send_sync(sp.Serialize())
	}
}

// 處理重傳
func (sc *SerialComm) handleRetransmit(ackPacket *AckPacket) {
	// 使用最後收到的 Ack 信息來決定哪些包需要重傳
	// 如果某個包在緩衝區中存在但未被 Ack（對應的 bitmap 位為 0），則重傳它

	// 遍歷Bitmap中的所有位，补发缺失的包
	for i := uint8(0); i < ackPacket.ReceiveLen; i++ {
		seqNum := ackPacket.BaseSeq + uint32(i) + 1
		// 檢查該序號是否在發送窗口內
		if (ackPacket.Bitmap & (1 << i)) == 0 {
			if packet, exists := sc.sendBuffer[seqNum]; exists {
				data := packet.Serialize(sc.debug)
				if sc.debug > 0 {
					log.Printf("port %s Retransmitting packet seq: %d", sc.portName, seqNum)
				}
				sc.Ser_Write("Retransmit", data, false)
			}
		}
	}
}

// 接收循環 - 處理分片接收和包同步
func (sc *SerialComm) receiveLoop() {
	tempBuffer := make([]byte, 1024)

	for {
		select {
		case <-sc.done:
			return
		default:
			n, err := sc.port.Read(tempBuffer)
			if err != nil {
				log.Printf("Read error: %v", err)
				continue
			}

			if n > 0 {
				sc.processIncomingBytes(tempBuffer[:n])
			}
		}
	}
}

// 處理接收到的字節流，支持分片接收和包同步
func (sc *SerialComm) processIncomingBytes(data []byte) {
	rb := sc.receiveBuffer

	if sc.debug > 3 {
		if len(data) < 33 {
			fmt.Printf("%s %s -- %d %d\n", sc.portName, ToHex(data), len(data), len(rb.buffer))
		} else {
			fmt.Printf("%s %s ... %s -- %d %d\n", sc.portName, ToHex(data[:15]), ToHex(data[len(data)-15:]), len(data), len(rb.buffer))
		}
	}

	// 將新數據添加到接收緩衝區
	rb.buffer = append(rb.buffer, data...)

	for {
		if len(rb.buffer) < 4 {
			// 數據不足以檢查同步頭
			if sc.debug > 4 {
				log.Printf("port %s processIncomingBytes No enought rb.buffer\n", sc.portName)
			}
			break
		}

		// 查找同步模式
		// bytes.Index(rb.buffer, []byte(SYNC_PATTERN))
		// syncPos := sc.findSyncPattern(rb.buffer)
		syncPos := bytes.Index(rb.buffer, []byte(SYNC_HEADER))
		if syncPos == -1 {
			// 沒找到同步模式，保留最後3個字節（可能包含部分同步模式）
			if len(rb.buffer) > 3 {
				copy(rb.buffer, rb.buffer[len(rb.buffer)-3:])
				rb.buffer = rb.buffer[:3]
			}
			break
		}

		if sc.debug > 4 {
			log.Printf("port %s rb.buffer %d %d \n %s\n", sc.portName, syncPos, len(rb.buffer), ToHex(rb.buffer))
		}
		// 找到同步模式，丟棄之前的無效數據
		if syncPos >= 0 {
			copy(rb.buffer, rb.buffer[syncPos:])
			rb.buffer = rb.buffer[:len(rb.buffer)-syncPos]
		}

		// 檢查是否有足夠的數據來讀取包頭
		if len(rb.buffer) < 19 { // 最小頭部大小: 4+1+4+2+2+1+1+4 = 19 (數據包)
			break
		}

		// 讀取包類型和長度信息
		packetType := rb.buffer[4]
		if packetType == PACKET_TYPE_DATA {
			// 數據包
			if len(rb.buffer) < 19 { // 數據包最小頭部大小
				break
			}

			dataLen := binary.LittleEndian.Uint16(rb.buffer[9:11])
			parityLen := binary.LittleEndian.Uint16(rb.buffer[11:13])
			totalLen := PACKET_DATA_SIZE + int(dataLen) + int(parityLen) // 數據包總長度(含尾部同步字)
			if len(rb.buffer) >= totalLen {
				// 完整的數據包
				if sc.debug > 4 {
					log.Printf("port %s dataLen %d parityLen %d totalLen %d \n", sc.portName, dataLen, parityLen, totalLen)
				}

				packetData := make([]byte, totalLen)
				copy(packetData, rb.buffer[:totalLen])

				// 從緩衝區移除已處理的數據
				copy(rb.buffer, rb.buffer[totalLen:])
				rb.buffer = rb.buffer[:len(rb.buffer)-totalLen]
				if sc.debug > 4 {
					log.Printf("port %s rb.buffer left %d \n", sc.portName, len(rb.buffer))
				}
				// 處理數據包
				sc.handleDataPacket(packetData)
				if sc.debug > 4 {
					log.Printf("port %s sc.handleDataPacket return \n", sc.portName)
				}
			} else {
				// 包不完整，等待更多數據
				break
			}
		} else if packetType == PACKET_TYPE_ACK {
			// 確認包 (固定31字節)
			if len(rb.buffer) >= PACKET_ACK_SIZE_WITH_TAIL {
				ackData := make([]byte, PACKET_ACK_SIZE_WITH_TAIL)
				copy(ackData, rb.buffer[:PACKET_ACK_SIZE_WITH_TAIL])

				// 從緩衝區移除已處理的數據
				copy(rb.buffer, rb.buffer[PACKET_ACK_SIZE_WITH_TAIL:])
				rb.buffer = rb.buffer[:len(rb.buffer)-PACKET_ACK_SIZE_WITH_TAIL]

				// 處理確認包
				sc.handleAck(ackData)
			} else {
				// 確認包不完整
				break
			}
		} else {
			// 無效的包類型，跳過這個同步頭
			rb.buffer = rb.buffer[1:]
		}
	}
}

// 處理數據包
func (sc *SerialComm) handleDataPacket(data []byte) bool {
	sc.mutex.Lock()
	defer sc.mutex.Unlock()

	send_ack_func := func() {
		// 確認頻率 = 窗口大小 / 4
		// Bitmap 为 64 时，也至少要收到 4个包才可能发 ack 返回
		// ------**** 非常深层次的问题 ****------
		// 还没有考虑 小于 4个包时，如何通知 对侧？（一个定时任务，或者根据 丢失无效的同步头计数？）
		// 要有一個定時器，用來在沒有 達到4個包時，還能强行發 ack
		// 另一方面，发送方在发送完 16 个（windows 长度）后，没有收到 ack，应该怎么办？
		if sc.ACK_Count == sc.windowSize/4 {
			sc.ackLoop_reset_Chan <- struct{}{} // 重置 ack_Loop 倒計時
			sc.ACK_Count = 0
			sc.sendAck()
		}
	}

	packet, err := DeserializePacket(data)
	if err != nil {
		if sc.debug > 0 {
			log.Printf("Failed to deserialize packet: %v", err)
		}
		return false
	}

	sc.ACK_Count++
	if sc.firstRecvBool {
		// 第一次收到數據包
		sc.firstRecvBool = false
		sc.recvBase = packet.SeqNum - 1
		sc.recvMax = packet.SeqNum
	} else if (packet.SeqNum - sc.recvMax) < 0xFFFF_FFFF/2 {
		// 如果新收到的數據包 在 recvMax 後面，更新 recvMax
		sc.recvMax = packet.SeqNum
	}

	// 檢查數據包是否已經存在於接收緩衝區中（避免重複處理）
	if _, exists := sc.recvWindow[packet.SeqNum]; exists {
		if sc.debug > 0 {
			log.Printf("Duplicate packet %d received, ignoring", packet.SeqNum)
		}
		send_ack_func()
		return false
	}

	// 驗證校驗和
	// 根据数据包中的配置创建 Reed-Solomon 解码器
	dataShards := int(packet.DataShards)
	parityShards := int(packet.ParityShards)

	var rsEncoder reedsolomon.Encoder

	// 如果配置与本地不同，则创建新的编码器
	if dataShards != sc.dataShards || parityShards != sc.parityShards {
		rsEncoder, err = reedsolomon.New(dataShards, parityShards)
		if err != nil {
			if sc.debug > 0 {
				log.Printf("Failed to create Reed-Solomon encoder for packet %d: %v", packet.SeqNum, err)
			}
			send_ack_func()
			return false
		}
	} else {
		rsEncoder = sc.rsEncoder
	}

	// 重構Reed-Solomon分片
	shardSize := (len(packet.Data) + dataShards - 1) / dataShards
	totalSize := shardSize * dataShards

	// 填充數據到總大小
	paddedData := make([]byte, totalSize)
	copy(paddedData, packet.Data)

	// 創建分片
	shards := make([][]byte, dataShards+parityShards)

	// 填充數據分片
	for i := 0; i < dataShards; i++ {
		shards[i] = paddedData[i*shardSize : (i+1)*shardSize]
	}

	// 填充校驗分片
	parityShardSize := len(packet.Parity) / parityShards
	for i := 0; i < parityShards; i++ {
		start := i * parityShardSize
		end := start + parityShardSize
		if end > len(packet.Parity) {
			end = len(packet.Parity)
		}
		shards[dataShards+i] = make([]byte, parityShardSize)
		copy(shards[dataShards+i], packet.Parity[start:end])
	}

	// 使用Reed-Solomon解碼
	correctedData, err := sc.decodeFECWithEncoder(rsEncoder, shards, len(packet.Data), dataShards)
	if err != nil {
		if sc.debug > 0 {
			log.Printf("Reed-Solomon decode failed for packet %d: %v", packet.SeqNum, err)
		}
		send_ack_func()
		return false
	}

	// 更新包數據
	packet.Data = correctedData

	// serialized := packet.Serialize()
	// expectedChecksum := calculateChecksum(serialized[4 : len(serialized)-8]) // 跳過同步頭和校驗和，與發送端保持一致
	expectedChecksum := calculateChecksum(packet.Data)
	if packet.Checksum != expectedChecksum {
		log.Printf("--- port %s Checksum mismatch for packet %d", sc.portName, packet.SeqNum)
		send_ack_func()
		return false
	}

	// 存儲到接收緩衝區
	sc.recvWindow[packet.SeqNum] = packet

	// 處理接收窗口
	go sc.handleReceiveData(packet)
	send_ack_func()
	return true
}

// 更新接收窗口
func (sc *SerialComm) handleReceiveData(packet *Packet) {
	if sc.debug > 0 {
		log.Printf("port %s Received packet %d len %d  \n", sc.portName, packet.SeqNum, len(packet.Data))
	}
	sp, _ := Deserialize_SP_Packet(packet.Data)
	if sp != nil {
		// 获取或创建SP包序列
		spList, exists := sc.sp_packet[sp.SP_Seq]
		if !exists || spList == nil {
			spList = make([]*SP_Packet, 0, sp.SP_Length)
			sc.sp_packet[sp.SP_Seq] = spList
		}

		// 添加新分片
		sc.sp_packet[sp.SP_Seq] = append(spList, sp)
		spList = sc.sp_packet[sp.SP_Seq] // 更新引用

		// 检查是否已接收完整序列
		if len(spList) == int(sp.SP_Length) {
			// 所有分片都已接收，進行重組
			fullPacket := Concat_SP_Packets(spList)
			if fullPacket != nil {
				sc.recvChan <- sc.ungzip_data(fullPacket)
			}
			// 清理已處理的分片
			delete(sc.sp_packet, sp.SP_Seq)
		}
	} else {
		sc.recvChan <- sc.ungzip_data(packet.Data)
	}
}

// ack 循環（在沒有正常發送 ack 時定時觸發）
func (sc *SerialComm) ackLoop() {
	time_wait := time.Duration(1000 * time.Millisecond)
	// 创建一个可重置的定时器
	timer := time.NewTimer(time_wait)
	defer timer.Stop()

	// 重置信号通道

	for {
		select {
		case <-sc.done:
			return
		case <-sc.ackLoop_reset_Chan:
			// 重置定时器
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(time_wait)
			// if sc.debug > 2 {
			// 	log.Printf("port %s ACK timer reset", sc.portName)
			// }
		case <-timer.C:
			if (sc.recvMax - sc.recvBase) < 0xFFFF_FFFF/2 {
				if sc.debug > 2 {
					log.Printf("port %s ACK timer triggered", sc.portName)
				}
				sc.sendAck()
			}
			timer.Reset(time_wait)
		}
	}
}

func (sc *SerialComm) circleSendLoop() {
	// 2 倍的 發送 windowSize /4 數據（1024）時間 作爲 為收到回應的 數據重發
	time_Wait := time.Duration(math.Round(1.10 * 2 * float64(sc.windowSize) / 4 * 1024 * 10 / float64(sc.BaudRate)))
	// 创建一个可重置的定时器
	timer := time.NewTimer(time_Wait)
	defer timer.Stop()

	for {
		select {
		case <-sc.done:
			return
		case <-sc.circleSend_reset_Chan:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(time_Wait)
		case <-timer.C:
			// 沒有收到任何ack包，要警覺了，開始循環發送 sendWindow數據
			for i := sc.sendBase; i < sc.sendNext; i++ {
				packet, exists := sc.sendBuffer[i]
				if exists && packet != nil {
					data := packet.Serialize(sc.debug)
					sc.Ser_Write("cDATA", data, false)
				}
			}
		}
	}
}

// 發送確認包
func (sc *SerialComm) sendAck() {
	ackPacket := &AckPacket{
		SyncHeader: SYNC_PATTERN,
		PacketType: PACKET_TYPE_ACK,
		// recvBase 窗口第一個值的前一個序號
		// 若前面有 recvMax 都是無效的包，ReceiveLen 能正確計數
		// 若存在有效的包，Bitmap 也能正確計數
		BaseSeq:      sc.recvBase,
		ReceiveLen:   uint8(sc.recvMax - sc.recvBase),
		Bitmap:       0,
		DataShards:   uint8(sc.dataShards),
		ParityShards: uint8(sc.parityShards),
		AckSeqNum:    sc.ackSeqNum, // 設置ACK序號
		SyncTail:     SYNC_TAIL,
	}

	// 设置位图并计算连续接收的包数量
	consecutiveReceived := uint32(0)
	for i := uint32(0); i < 64; i++ {
		seqNum := ackPacket.BaseSeq + i + 1
		if _, exists := sc.recvWindow[seqNum]; exists {
			ackPacket.Bitmap |= (1 << i)
			// 只有当之前的所有包都收到时，才增加连续计数
			if i == consecutiveReceived {
				consecutiveReceived++
			}
		}
	}

	serialized := ackPacket.Serialize()
	ackPacket.Checksum = calculateChecksum(serialized[4 : len(serialized)-8]) // 跳過同步頭和校驗和

	finalData := ackPacket.Serialize()
	if sc.debug > 2 {
		log.Printf("port %s Sent ACK #%d: base seq %d %d (current recvBase: %d) with bitmap 0x%X ",
			sc.portName, ackPacket.AckSeqNum,
			ackPacket.BaseSeq, ackPacket.ReceiveLen,
			sc.recvBase, ackPacket.Bitmap)
	}

	// 遞增ACK序號
	sc.ackSeqNum++

	sc.Ser_Write("ACK", finalData, true)

	for i := 0; i < int(consecutiveReceived); i++ {
		delete(sc.recvWindow, sc.recvBase+uint32(i))
	}
	if sc.debug > 2 {
		log.Printf("port %s Updated recvBase from %d to %d ", sc.portName, sc.recvBase, sc.recvBase+uint32(consecutiveReceived))
	}
	sc.recvBase += consecutiveReceived
}

// 處理確認包
func (sc *SerialComm) handleAck(data []byte) {
	sc.mutex.Lock()
	defer sc.mutex.Unlock()

	ackPacket, err := DeserializeAckPacket(sc.portName, data)
	if err != nil {
		log.Printf("%s Failed to deserialize ack packet: %v", sc.portName, err)
		return
	}

	if sc.debug > 2 {
		log.Printf("port %s Received ACK #%d: base seq %d with bitmap 0x%X",
			sc.portName, ackPacket.AckSeqNum, ackPacket.BaseSeq, ackPacket.Bitmap)
	}

	consecutiveReceived := ackPacket.BaseSeq
	// 根據確認信息清理發送緩衝區
	for i := uint8(0); i < ackPacket.ReceiveLen; i++ {
		seqNum := ackPacket.BaseSeq + uint32(i) + 1
		if (ackPacket.Bitmap & (1 << i)) != 0 {
			// 該包已確認，從發送緩衝區移除
			delete(sc.sendBuffer, seqNum)
			if consecutiveReceived == seqNum {
				consecutiveReceived++
			}
			// 該包未確認，不從發送緩衝區移除
		}
	}
	sc.sendBase = consecutiveReceived
	if sc.debug > 2 {
		log.Printf("port %s Received ACK for base seq %d with bitmap 0x%X %d cont %d\n", sc.portName, ackPacket.BaseSeq, ackPacket.Bitmap, sc.sendNext, consecutiveReceived)
		if consecutiveReceived > sc.ackSeqNum {
			log.Printf("port %s Updated sendBase to %d ", sc.portName, sc.sendBase)
		}
	}
	//收到了ack包，可以重置 circleSendLoop 定時器了
	sc.circleSend_reset_Chan <- struct{}{}

	// 自適應調整Reed-Solomon參數
	sc.adaptRSParameters(ackPacket)

	sc.handleRetransmit(ackPacket)
}

// 自適應調整Reed-Solomon參數
func (sc *SerialComm) adaptRSParameters(ackPacket *AckPacket) {
	// 計算成功率
	successCount := 0

	// 遍歷Bitmap中的所有位
	for i := uint8(0); i < ackPacket.ReceiveLen; i++ {
		// 檢查該序號是否在發送窗口內
		if (ackPacket.Bitmap & (1 << i)) != 0 {
			successCount++
		}
	}
	if sc.debug > 2 {
		log.Printf("port %s Success rate: (%d/%d) %d", sc.portName, successCount, ackPacket.ReceiveLen, sc.sendNext)
	}

	if ackPacket.ReceiveLen > 0 {
		successRate := float32(successCount) / float32(ackPacket.ReceiveLen)
		sc.errorRate = 1.0 - successRate

		// 根據錯誤率調整Reed-Solomon參數
		if sc.errorRate < 0.01 { // 錯誤率低於1%
			// 考慮減少校驗分片數 (但保持最小值)
			if sc.parityShards > 1 {
				newParityShards := max(1, sc.parityShards-1)
				if newEncoder, err := reedsolomon.New(sc.dataShards, newParityShards); err == nil {
					sc.parityShards = newParityShards
					sc.rsEncoder = newEncoder
					if sc.debug > 1 {
						log.Printf("port %s Reduced parity shards to %d %e %d %d  ", sc.portName, sc.parityShards, sc.errorRate, successCount, ackPacket.ReceiveLen)
					}
				}
			}
		} else if sc.errorRate > 0.05 { // 錯誤率高於5%
			// 增加校驗分片數
			if sc.parityShards < 4 {
				newParityShards := min(4, sc.parityShards+1)
				if newEncoder, err := reedsolomon.New(sc.dataShards, newParityShards); err == nil {
					sc.parityShards = newParityShards
					sc.rsEncoder = newEncoder
					if sc.debug > 1 {
						log.Printf("port %s Increased parity shards to %d %e %d %d  ", sc.portName, sc.parityShards, sc.errorRate, successCount, ackPacket.ReceiveLen)
					}
				}
			}
		}
	}
	if sc.debug > 2 {
		log.Printf("port %s Adapted Reed-Solomon parameters: %d %e %d %d", sc.portName, sc.parityShards, sc.errorRate, successCount, ackPacket.ReceiveLen)
	}
}
