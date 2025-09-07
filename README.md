# Serial port file transmitter

## Features
 
1. Strong verification and self-recovery of serial port data

2. Remote file browser

3. Segmented file transfer

4. Support multiple operating systems (go fyne)

## Introduction to Source Code

1. Main guidance
gmain.go

2. Serial communication and testing
go_serial_comm.go
go_serial_comm_main.go

3. Remote file browsing
rpc_serail.go
rpc_osStat_def.go
rpc_net.go

4. gui interface
gui_main.go
gui_theme.go

## Precautions

Serial communication consists of 8 data bits, 1 stop bit, no check, and no flow control
