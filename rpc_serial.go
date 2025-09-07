// rpc_serial.go
package main

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
	"runtime"
	"sync"
	"time"
)

// RPCPacketType 定義 RPC 包的類型
type RPCPacketType int

// 在 RPCPacketType 中添加新的包类型定义
const (
	RPCRequest    RPCPacketType = iota // 請求包
	RPCResponse                        // 響應包
	RPCDataLength                      // 數據長度包
)

// DataLengthPacket 代表數據長度包
type DataLengthPacket struct {
	ID     uint64 // 請求/響應ID
	Length int    // 數據包長度
}

// RPCPacket 代表 RPC 請求和響應包
type RPCPacket struct {
	Type     RPCPacketType // 包類型
	ID       uint64        // 請求/響應ID
	FuncName string        // 函數名稱（僅請求包使用）
	Args     []any         // 函數參數（僅請求包使用）
	Result   []any         // 函數返回值（僅響應包使用）
	Error    string        // 錯誤信息（僅響應包使用）
}

// HandshakePacket 代表握手包
type HandshakePacket struct {
	IsHandshake bool // 标识这是一个握手包
}

// 修改 SerialRPCNode 结构体，添加一个用于接收数据长度信息的通道
type SerialRPCNode struct {
	functions         map[string]reflect.Value
	mu                sync.RWMutex
	serialComm        *SerialComm
	started           chan struct{}
	isRunning         bool
	pending           map[uint64]chan RPCPacket
	pendingDataLength map[uint64]chan int // 用于等待数据长度信息
	nextID            uint64
	timeout           time.Duration
	once              sync.Once     // 确保只关闭一次
	handshaked        bool          // 握手状态
	handshakeChan     chan struct{} // 握手完成通知
}

// 修改 NewSerialRPCNode 初始化新的字段
func NewSerialRPCNode(portName string, baudRate int, debug int) (*SerialRPCNode, error) {
	// 注册 gob 类型
	gob.Register([]DirEntryData{})
	gob.Register(DirEntryData{})
	gob.Register(FileInfoData{})
	gob.Register(RPCPacket{})
	gob.Register(HandshakePacket{})  // 注册握手包类型
	gob.Register(DataLengthPacket{}) // 注册数据长度包类型

	// 初始化串口通信
	serialComm, err := NewSerialComm(portName, baudRate, debug)
	if err != nil {
		return nil, fmt.Errorf("无法初始化串口通信: %w", err)
	}

	node := &SerialRPCNode{
		functions:         make(map[string]reflect.Value),
		serialComm:        serialComm,
		started:           make(chan struct{}),
		isRunning:         true,
		pending:           make(map[uint64]chan RPCPacket),
		pendingDataLength: make(map[uint64]chan int),
		timeout:           50 * time.Second,
		handshakeChan:     make(chan struct{}),
	}

	// 启动接收循环
	go node.receiveLoop()

	// 启动握手过程
	go node.handshakeProcess()

	return node, nil
}

// 修改 handlePacket 方法以处理数据长度包并通知等待的 Call 方法
func (n *SerialRPCNode) handlePacket(data []byte) {
	decoder := gob.NewDecoder(bytes.NewBuffer(data))

	// 首先嘗試解碼為握手包
	var handshakePacket HandshakePacket
	if err := decoder.Decode(&handshakePacket); err == nil && handshakePacket.IsHandshake {
		// 收到握手包，設置握手狀態
		if !n.handshaked {
			fmt.Println("收到握手包")
			n.sendHandshake()
			n.handshaked = true
			close(n.handshakeChan)
		}
		return
	}

	// 嘗試解碼為數據長度包
	var lengthPacket DataLengthPacket
	decoder = gob.NewDecoder(bytes.NewBuffer(data)) // 重新創建解碼器
	if err := decoder.Decode(&lengthPacket); err == nil && lengthPacket.Length > 0 {
		// 通知等待该数据长度的 Call 方法
		n.mu.Lock()
		if lengthChan, exists := n.pendingDataLength[lengthPacket.ID]; exists {
			// 发送数据长度信息
			select {
			case lengthChan <- lengthPacket.Length:
			default:
				// 如果通道已满或已关闭，忽略
			}
			// 删除已处理的条目
			delete(n.pendingDataLength, lengthPacket.ID)
		}
		n.mu.Unlock()

		fmt.Printf("收到數據長度包，ID: %d, Length: %d\n", lengthPacket.ID, lengthPacket.Length)
		return
	}

	// 如果不是握手包和數據長度包，則處理為RPC包
	var packet RPCPacket
	decoder = gob.NewDecoder(bytes.NewBuffer(data)) // 重新創建解碼器
	if err := decoder.Decode(&packet); err != nil {
		fmt.Printf("解碼數據包失敗: %v\n", err)
		return
	}

	switch packet.Type {
	case RPCRequest:
		n.handleRequest(packet)
	case RPCResponse:
		n.handleResponse(packet)
	default:
		fmt.Printf("未知的數據包類型: %v\n", packet.Type)
	}
}

// 修改 Call 方法，异步等待数据长度信息
func (n *SerialRPCNode) Call(funcName string, args ...any) ([]any, error) {
	// 等待握手完成
	n.WaitForHandshake()

	n.mu.Lock()
	id := n.nextID
	n.nextID++
	respChan := make(chan RPCPacket, 1)
	n.pending[id] = respChan

	// 创建用于接收数据长度信息的通道
	lengthChan := make(chan int, 1)
	n.pendingDataLength[id] = lengthChan
	n.mu.Unlock()

	// 准备请求包
	packet := RPCPacket{
		Type:     RPCRequest,
		ID:       id,
		FuncName: funcName,
		Args:     args,
	}

	fmt.Printf("发送请求: %+v\n", packet)
	// 发送请求
	n.sendPacket(packet)

	// 等待响应或超时
	var timeout = n.timeout
	var timer = time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case resp := <-respChan:
			n.mu.Lock()
			delete(n.pending, id)
			delete(n.pendingDataLength, id)
			n.mu.Unlock()

			if resp.Error != "" {
				return nil, errors.New(resp.Error)
			}
			return resp.Result, nil

		case length := <-lengthChan:
			// 收到数据长度信息，重新计算超时时间
			// 根据数据长度估算超时时间，这里简单地根据数据大小增加超时时间
			// 每KB数据增加1秒超时时间(xxxx)
			additionalTimeout := time.Duration(3*length*10/n.serialComm.BaudRate) * time.Second
			if additionalTimeout > 0 {
				// timeout = n.timeout + additionalTimeout
				timeout = additionalTimeout
				// 重置定时器
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(timeout)
				fmt.Printf("根据数据长度调整超时时间: %v (数据长度: %d)\n", timeout, length)
			}

		case <-timer.C:
			n.mu.Lock()
			delete(n.pending, id)
			delete(n.pendingDataLength, id)
			n.mu.Unlock()
			return nil, errors.New("請求超時")
		}
	}
}

// 修改 sendPacket 方法，添加對大數據包的處理
func (n *SerialRPCNode) sendPacket(packet RPCPacket) {
	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)

	if err := encoder.Encode(packet); err != nil {
		fmt.Printf("編碼數據包失敗: %v %+v\n", err, packet)
		return
	}

	data := buf.Bytes()
	// 如果數據包較大(比如超過1024字節)，先發送數據長度包
	const largeDataThreshold = 1024 * 8
	if len(data) > largeDataThreshold {
		// 先發送數據長度包
		lengthPacket := DataLengthPacket{
			ID:     packet.ID,
			Length: len(data),
		}

		var lengthBuf bytes.Buffer
		lengthEncoder := gob.NewEncoder(&lengthBuf)

		if err := lengthEncoder.Encode(lengthPacket); err != nil {
			fmt.Printf("編碼數據長度包失敗: %v\n", err)
			return
		}

		// 發送數據長度包
		n.serialComm.Send_Data(lengthBuf.Bytes())
		// 稍微延遲一下確保接收方處理完長度包
		time.Sleep(10 * time.Millisecond)
	}

	// 發送實際的數據包
	n.serialComm.Send_Data(data)
}

// handshakeProcess 处理握手过程
func (n *SerialRPCNode) handshakeProcess() {
	// 如果已经握手完成，直接返回
	if n.handshaked {
		return
	}
	n.sendHandshake()
	n.sendHandshake()
	n.sendHandshake()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// 发送握手包
			n.sendHandshake()
		case <-n.handshakeChan:
			// 握手完成
			fmt.Println("握手完成，开始正常通信")
			return
		case <-n.serialComm.done:
			// 串口关闭
			return
		}
	}
}

// sendHandshake 发送握手包
func (n *SerialRPCNode) sendHandshake() {
	handshakePacket := HandshakePacket{
		IsHandshake: true,
	}

	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)

	if err := encoder.Encode(handshakePacket); err != nil {
		fmt.Printf("编码握手包失败: %v\n", err)
		return
	}

	// 使用串口发送握手数据
	n.serialComm.Send_Data(buf.Bytes())
}

// WaitForHandshake 等待握手完成
func (n *SerialRPCNode) WaitForHandshake() {
	<-n.handshakeChan
}

// Start 啟動 RPC 節點
func (n *SerialRPCNode) Start() error {
	close(n.started) // 通知節點已啟動
	fmt.Printf("串口 RPC 節點已啟動\n")
	return nil
}

// WaitForStart 等待節點啟動完成
func (n *SerialRPCNode) WaitForStart() {
	<-n.started
}

// Register 註冊一個函數到 RPC 節點
func (n *SerialRPCNode) Register(name string, f interface{}) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	fVal := reflect.ValueOf(f)
	if fVal.Kind() != reflect.Func {
		return errors.New("註冊的必須是函數")
	}
	if _, exists := n.functions[name]; exists {
		return errors.New("函數已存在")
	}
	n.functions[name] = fVal
	return nil
}

// receiveLoop 處理從串口接收的數據包
func (n *SerialRPCNode) receiveLoop() {
	for n.isRunning {
		select {
		case data := <-n.serialComm.recvChan:
			go n.handlePacket(data)
		case <-n.serialComm.done:
			return
		}
	}
}

// handleRequest 处理请求包
func (n *SerialRPCNode) handleRequest(packet RPCPacket) {
	n.mu.RLock()
	fVal, ok := n.functions[packet.FuncName]
	n.mu.RUnlock()

	response := RPCPacket{
		Type: RPCResponse,
		ID:   packet.ID,
	}

	if !ok {
		response.Error = "函数未找到"
	} else {
		// 准备参数
		fType := fVal.Type()
		inArgs := make([]reflect.Value, len(packet.Args))
		if fType.NumIn() != len(packet.Args) {
			response.Error = "参数数量不匹配"
		} else {
			for i := range packet.Args {
				// 检查参数是否为 nil
				if packet.Args[i] == nil {
					// 对于 nil 参数，创建对应类型的零值
					inArgs[i] = reflect.Zero(fType.In(i))
					continue
				}

				inArgs[i] = reflect.ValueOf(packet.Args[i])
				if !inArgs[i].Type().ConvertibleTo(fType.In(i)) {
					response.Error = fmt.Sprintf("参数类型不匹配 at %d", i)
					break
				}
				inArgs[i] = inArgs[i].Convert(fType.In(i))
			}

			if response.Error == "" {
				// 调用函数
				out := fVal.Call(inArgs)
				response.Result = make([]any, len(out))

				// 检查函数是否返回错误作为最后一个返回值
				if fType.NumOut() > 0 {
					lastOutIndex := fType.NumOut() - 1
					lastOutType := fType.Out(lastOutIndex)
					// 检查最后一个返回值是否实现了 error 接口
					if lastOutType.Implements(reflect.TypeOf((*error)(nil)).Elem()) &&
						!out[lastOutIndex].IsNil() {
						if err, ok := out[lastOutIndex].Interface().(error); ok && err != nil {
							response.Error = err.Error()
							// 清空结果，因为有错误
							response.Result = nil
						}
					}
				}

				// 只有在没有错误的情况下才处理其他返回值
				if response.Error == "" {
					for i := range out {
						// 处理特殊返回值类型
						switch v := out[i].Interface().(type) {
						case []fs.DirEntry:
							data, err := ToDirEntryData(v)
							if err != nil {
								response.Error = fmt.Sprintf("序列化 DirEntry 失败: %v", err)
								break
							} else {
								response.Result[i] = data
							}
						case fs.FileInfo:
							response.Result[i] = ToFileInfoData(v)
						default:
							response.Result[i] = v
						}
					}
				}
			}
		}
	}

	// 发送响应
	n.sendPacket(response)
}

// handleResponse 處理響應包
func (n *SerialRPCNode) handleResponse(packet RPCPacket) {
	n.mu.Lock()
	respChan, exists := n.pending[packet.ID]
	n.mu.Unlock()

	if exists {
		respChan <- packet
	}
}

// Close 關閉 RPC 節點
func (n *SerialRPCNode) Close() error {
	// 使用 Once 確保只執行一次
	n.once.Do(func() {
		// n.Stop()
		n.isRunning = false
		if n.serialComm != nil {
			n.serialComm.Close()
		}
	})
	return nil
}

// GetFileSplit 从指定文件的指定位置读取指定长度的内容
func GetFileSplit(filename string, offset int64, length int64) ([]byte, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("无法打开文件 %s: %w", filename, err)
	}
	defer file.Close()

	// 定位到指定的偏移位置
	_, err = file.Seek(offset, 0)
	if err != nil {
		return nil, fmt.Errorf("无法定位到文件位置 %d: %w", offset, err)
	}

	// 创建缓冲区并读取指定长度的数据
	data := make([]byte, length)
	n, err := file.Read(data)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("读取文件时出错: %w", err)
	}

	// 如果读取的字节数少于预期，调整切片大小
	return data[:n], nil
}

// IsWindows 判断当前系统是否为Windows
func OsIsWindows() bool {
	return runtime.GOOS == "windows"
}

// 示例使用
func rpc_serial_main() {
	// 創建節點A
	nodeA, err := NewSerialRPCNode("COM6", 115200, 0) // 根據實際情況修改端口名和波特率
	if err != nil {
		fmt.Printf("創建節點A失敗: %v\n", err)
		return
	}
	defer nodeA.Close()

	// 創建節點B
	nodeB, err := NewSerialRPCNode("COM9", 115200, 0) // 根據實際情況修改端口名和波特率
	if err != nil {
		fmt.Printf("創建節點B失敗: %v\n", err)
		return
	}
	defer nodeB.Close()

	// 注册函数到节点A
	if err := nodeA.Register("ReadDir", os.ReadDir); err != nil {
		fmt.Printf("节点A注册 ReadDir 失败: %v\n", err)
		return
	}
	if err := nodeA.Register("Stat", os.Stat); err != nil {
		fmt.Printf("节点A注册 Stat 失败: %v\n", err)
		return
	}
	// 添加这行注册新的GetFileSplit函数
	if err := nodeA.Register("GetFileSplit", GetFileSplit); err != nil {
		fmt.Printf("节点A注册 GetFileSplit 失败: %v\n", err)
		return
	}
	if err := nodeA.Register("OsIsWindows", OsIsWindows); err != nil {
		fmt.Printf("节点A注册 OsIsWindows 失败: %v\n", err)
		return
	}

	// 注册函数到节点B
	if err := nodeB.Register("ReadDir", os.ReadDir); err != nil {
		fmt.Printf("节点B注册 ReadDir 失败: %v\n", err)
		return
	}
	if err := nodeB.Register("Stat", os.Stat); err != nil {
		fmt.Printf("节点B注册 Stat 失败: %v\n", err)
		return
	}
	// 添加这行注册新的GetFileSplit函数
	if err := nodeB.Register("GetFileSplit", GetFileSplit); err != nil {
		fmt.Printf("节点B注册 GetFileSplit 失败: %v\n", err)
		return
	}
	if err := nodeB.Register("OsIsWindows", OsIsWindows); err != nil {
		fmt.Printf("节点B注册 OsIsWindows 失败: %v\n", err)
		return
	}

	// 啟動節點
	go nodeA.Start()
	go nodeB.Start()

	// 等待節點啟動
	nodeA.WaitForStart()
	nodeB.WaitForStart()
	time.Sleep(200 * time.Millisecond)

	// 等待握手完成
	fmt.Println("等待节点握手完成...")
	nodeA.WaitForHandshake()
	nodeB.WaitForHandshake()
	fmt.Println("节点握手完成")

	// 節點A調用節點B的函數
	fmt.Println("節點A調用節點B的 ReadDir...")
	results, err := nodeA.Call("ReadDir", ".")
	if err != nil {
		fmt.Printf("調用 ReadDir 失敗: %v\n", err)
		return
	}
	dirEntries := results[0].([]DirEntryData)
	for _, entry := range dirEntries {
		fmt.Printf("DirEntry: %s, IsDir: %v\n", entry.Name, entry.IsDir)
	}

	// 節點B調用節點A的函數
	fmt.Println("節點B調用節點A的 Stat...")
	// results, err = nodeB.Call("Stat", "rpc_serial.go")
	results, err = nodeB.Call("Stat", "C:\\")
	if err != nil {
		fmt.Printf("調用 Stat 失敗: %v\n", err)
		return
	}
	fileInfo := results[0].(FileInfoData)
	fmt.Printf("FileInfo: %s, Size: %d, IsDir: %v\n", fileInfo.Name, fileInfo.Size, fileInfo.IsDir)

	// 节点A调用节点B的GetFileSplit函数
	fmt.Println("节点A调用节点B的 GetFileSplit...")
	results, err = nodeA.Call("GetFileSplit", "rpc_serial.go", int64(0), int64(100))
	if err != nil {
		fmt.Printf("调用 GetFileSplit 失败: %v\n", err)
		return
	}
	fileContent := results[0].([]byte)
	fmt.Printf("读取到的内容: %s\n", string(fileContent))

	// 节点A调用节点B的OsIsWindows函数
	fmt.Println("节点A调用节点B的 OsIsWindows...")
	results, err = nodeA.Call("OsIsWindows")
	if err != nil {
		fmt.Printf("调用 OsIsWindows 失败: %v\n", err)
		return
	}
	isWindows := results[0].(bool)
	fmt.Printf("节点B系统是Windows: %v\n", isWindows)

	// 节点B调用节点A的OsIsWindows函数
	fmt.Println("节点B调用节点A的 OsIsWindows...")
	results, err = nodeB.Call("OsIsWindows")
	if err != nil {
		fmt.Printf("调用 OsIsWindows 失败: %v\n", err)
		return
	}
	isWindows = results[0].(bool)
	fmt.Printf("节点A系统是Windows: %v\n", isWindows)
}
