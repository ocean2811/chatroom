package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
)

/*
TODO: 增加发送表情的功能,利用命令行动画
*/

//User 客户端结构
type User struct {
	Addr string      //初始化后不可改变
	Name string      //读取实际值时加userMap读锁,写入时加写锁,初始化时不加锁
	C    chan []byte //生成后不可修改,只能由message关闭
	tips string      //读取实际值时加userMap读锁,写入时加写锁,初始化时不加锁
}

//TODO 解耦广播消息和指令消息的处理逻辑
var message = make(chan Message, 20)

//Message 广播/指令结构
type Message struct {
	sender *User
	data   string
	order  int
}

const (
	//ClientTips 客户端 命令提示符
	ClientTips = "[%s]:[%s]$ "
)

//OrderHelpList 指令的使用说明
var OrderHelpList string

//指令列表
const (
	//OrderInit 该条消息不是指令
	OrderInit = 0
	//OrderEmpty 用户发送了一条空消息
	OrderEmpty = 1 << iota
	//OrderLogin 登录消息
	OrderLogin
	//OrderLogout 退出消息
	OrderLogout
	//OrderList 列出在线用户
	OrderList
	//OrderAddr 返回该用户当前地址和昵称
	OrderAddr
	//OrderRename 修改昵称昵称
	OrderRename
	//OrderHelp 可用命令帮助
	OrderHelp
)

//UserMap 所有已连接的用户,读/写加锁
var UserMap struct { //TODO 分装成方法,不将锁暴漏到外界
	u map[string]*User
	sync.RWMutex
}

func main() {
	if len(os.Args) < 3 {
		fmt.Printf("usage: %s IP Port.\n", filepath.Base(os.Args[0]))
		os.Exit(1)
	}

	//生成命令提示列表
	b := bytes.NewBufferString("You can use:\n")
	tb := tabwriter.NewWriter(b, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tb, "\"addr\"\tQuery your network address and name.\n")
	fmt.Fprintf(tb, "\"list\"\tList online users\n")
	fmt.Fprintf(tb, "\"rename newname\"\tChange your nickname\n")
	fmt.Fprintf(tb, "\"help\"\tThe order help\n")
	tb.Flush()
	OrderHelpList = b.String()

	listener, err := net.Listen("tcp", net.JoinHostPort(os.Args[1], os.Args[2]))
	if err != nil {
		log.Fatalln(err)
	}
	defer listener.Close()

	go func() {
		UserMap.u = make(map[string]*User, 20)
		for msg := range message {
			if msg.order != OrderInit { //* 处理指令消息
				switch msg.order {
				case OrderEmpty:
					//TODO 空消息,可能用来探活
				case OrderLogin:
					msg.data = "User [" + msg.sender.Name + "] login!"
					UserMap.Lock()
					UserMap.u[msg.sender.Addr] = msg.sender
					UserMap.Unlock()
				case OrderLogout:
					msg.data = "User [" + msg.sender.Name + "] logout!"
					UserMap.Lock()
					close(msg.sender.C)
					delete(UserMap.u, msg.sender.Addr)
					UserMap.Unlock()
				case OrderList:
					buf := bytes.NewBufferString("User list:\n")
					table := tabwriter.NewWriter(buf, 0, 2, 2, ' ', 0)
					UserMap.RLock()
					for _, v := range UserMap.u {
						_, _ = table.Write([]byte("[" + v.Name + "]" + "\t[" + v.Addr + "]\n"))
					}
					UserMap.RUnlock()
					table.Flush()
					msg.data = buf.String()
				case OrderAddr:
					msg.data = "Your name is [" + msg.sender.Name + "],addr is [" + msg.sender.Addr + "]\n"
				case OrderRename:
					UserMap.Lock()
					msg.sender.Name = msg.data
					msg.sender.tips = fmt.Sprintf(ClientTips, msg.sender.Addr, msg.sender.Name)
					UserMap.Unlock()
					msg.data = "Change your name to [" + msg.sender.Name + "]\n"
				case OrderHelp:
					msg.data = OrderHelpList
				default:
					msg.data = "Unknow order: " + strconv.Itoa(msg.order)
				}
			}

			//除login/out外的指令:
			if msg.order != OrderInit && msg.order != OrderLogin && msg.order != OrderLogout {
				select {
				case msg.sender.C <- []byte(msg.data + msg.sender.tips):
				default:
					fmt.Printf("warning: lose msg. recv:[%s:%s],msg:[%s]\n", msg.sender.Name, msg.sender.Addr, msg.data)
				}
			} else { //其他消息和login/out指令
				var normal string
				if msg.order == OrderLogin || msg.order == OrderLogout {
					normal = makeUserMsg("", msg.data)
				} else {
					normal = makeUserMsg(msg.sender.Name, msg.data)
				}
				UserMap.RLock()
				for k, v := range UserMap.u {
					var d []byte
					if k == msg.sender.Addr {
						d = []byte(msg.sender.tips)
					} else {
						d = []byte(normal + v.tips)
					}
					select {
					case v.C <- d:
					default:
						fmt.Printf("warning: lose msg. recv:[%s:%s],msg:[%s]\n", v.Addr, v.Name, msg.data)
					}
				}
				UserMap.RUnlock()
			}
		}
	}()

	//接受链接
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Println(err)
			continue
		}
		go UserProc(conn)
	}
}

//makeUserMsg 拼接广播消息
func makeUserMsg(name string, msg string) string {
	if name == "" {
		return "\n" + msg + "\n"
	}
	return "\n<" + name + ">:" + msg + "\n"
}

//UserProc 处理客户端的读写
func UserProc(conn net.Conn) {
	defer conn.Close()

	cli := &User{conn.RemoteAddr().String(), conn.RemoteAddr().String(), make(chan []byte, 5), fmt.Sprintf(ClientTips, conn.RemoteAddr().String(), conn.RemoteAddr().String())}

	//广播上线消息
	message <- Message{sender: cli, order: OrderLogin}
	//执行一次help指令
	message <- Message{sender: cli, order: OrderHelp}

	go func() {
		//接受消息广播到聊天室
		data := make([]byte, 512)
		for {
			n, err := conn.Read(data)
			if err != nil || n <= 0 {
				if n <= 0 || err == io.EOF {
					message <- Message{sender: cli, order: OrderLogout}
					log.Printf("user logout. name=[%s],addr=[%s]\n", cli.Name, cli.Addr)
					break
				} else {
					log.Println(err)
					continue
				}
			}

			tmp := strings.TrimSpace(string(data[:n]))
			switch {
			case tmp == "": //空消息
				message <- Message{sender: cli, order: OrderEmpty}
			case tmp == "help":
				message <- Message{sender: cli, order: OrderHelp}
			case tmp == "list":
				message <- Message{sender: cli, order: OrderList}
			case tmp == "addr":
				message <- Message{sender: cli, order: OrderAddr}
			case strings.HasPrefix(tmp, "rename "):
				message <- Message{sender: cli, order: OrderRename, data: strings.TrimPrefix(tmp, "rename ")}
			default:
				message <- Message{sender: cli, data: strings.TrimRight(string(data[:n]), "\n\r")}
			}
		}
	}()

	//接受聊天室广播消息
	for msg := range cli.C {
		n, err := conn.Write(msg)
		if n <= 0 || err != nil {
			log.Println("n=", n, "err=", err)
			continue
		}
	}
}
