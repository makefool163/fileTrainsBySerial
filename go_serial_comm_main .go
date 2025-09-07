package main

import (
	"bytes"
	"math/rand"

	// "crypto/rand"
	"fmt"
	"io"
	"log"
	"os"
	"time"
)

func loop_recv(recvChan chan []byte, inputDataChan chan []byte, doneChan chan struct{}) {
	var data []byte
	var ret []byte
	// data := []byte{}
	// ret := []byte{}
	Count := 0
	for {
		select {
		case <-doneChan:
			return
		case data = <-inputDataChan:
			// Do something with ret
		case ret = <-recvChan:
			if bytes.Equal(data, ret) {
				fmt.Printf("Data %d OK\n", Count)
			} else {
				fmt.Printf("Data %d mismatch\n", Count)
			}
			Count += 1
		}
	}
}
func loop_test() {
	portA := os.Args[1]
	// baudrate := 9600
	// baudrate := 38400
	// baudrate := 57600
	baudrate := 115200
	// baudrate := 921600
	commA, _ := NewSerialComm(portA, baudrate, 0)

	doneChan := make(chan struct{})
	inputDataChan := make(chan []byte)
	go loop_recv(commA.recvChan, inputDataChan, doneChan)
	data := make([]byte, 1024*512)
	for i := range 60 {
		_, _ = rand.Read(data)
		fmt.Printf("data Sending packet %d\n", i)
		inputDataChan <- data
		commA.Send_sync(data)
		// commA.Send_Data_sync(data)
	}
	close(doneChan)
	commA.Close()

	time.Sleep(2 * time.Second) // 等待其他 goroutine 退出
}
func echo_comm(comm *SerialComm, doneChan chan struct{}) {
	for {
		select {
		case <-doneChan:
			return
		case data := <-comm.recvChan:
			comm.Send_Data_sync(data)
			// comm.Send_sync(data)
		}
	}
}

func ping_main() {
	portA := os.Args[1]
	portB := os.Args[2]
	// baudrate := 9600
	// baudrate := 38400
	// baudrate := 115200
	// baudrate := 460800
	baudrate := 921600
	commA, _ := NewSerialComm(portA, baudrate, 0)
	commB, _ := NewSerialComm(portB, baudrate, 0)

	doneChan := make(chan struct{})
	go echo_comm(commB, doneChan)

	data := make([]byte, 32*1024)
	for i := range data {
		data[i] = 0xFF
	}
	// data := make([]byte, 1024)
	for i := range 32 {
		_, _ = rand.Read(data)
		fmt.Printf("data Sending packet %d\n", i)
		// commA.Send_sync(data)
		commA.Send_Data_sync(data)
		retdata := <-commA.recvChan
		if !bytes.Equal(data, retdata) {
			log.Printf("Data mismatch detected")
		} else {
			log.Printf("Data \t%d OK\n", i)
		}
	}
	doneChan <- struct{}{}
	time.Sleep(1 * time.Second)
	commA.Close()
	commB.Close()
	time.Sleep(1 * time.Second) // 等待其他 goroutine 退出
}

func console_main() {
	// 打开日志文件
	file, err := os.OpenFile("d:/stk/app.log", os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		log.Fatal("无法打开日志文件:", err)
	}
	defer file.Close()

	// 创建一个同时写入文件和控制台的 writer
	multiWriter := io.MultiWriter(os.Stdout, file)

	// 设置 log 包的输出目标
	log.SetOutput(multiWriter)
	rand.Seed(time.Now().UnixNano())

	// console_main()
	// ping_main()
	// loop_test()
}
