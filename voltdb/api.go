package voltdb

import (
	"bytes"
	"sync"
	"fmt"
	"log"
	"net"
	"io"
)

// Conn is a single connection to a single node of a VoltDB database
type Conn struct {
	tcpConn  *net.TCPConn
	connData *connectionData
	channels map[int64]chan *bytes.Buffer
}

// connectionData are the values returned by a successful login.
type connectionData struct {
	hostId      int32
	connId      int64
	leaderAddr  int32
	buildString string
	connCount 	int64
	mu 		 	sync.Mutex
}

// NewConn creates an initialized, authenticated Conn.
func NewConnection(user string, passwd string, hostAndPort string) (*Conn, error) {
	var conn = new(Conn)
	var err error
	var raddr *net.TCPAddr
	var login bytes.Buffer

	if raddr, err = net.ResolveTCPAddr("tcp", hostAndPort); err != nil {
		return nil, fmt.Errorf("Error resolving %v.", hostAndPort)
	}
	if conn.tcpConn, err = net.DialTCP("tcp", nil, raddr); err != nil {
		return nil, err
	}
	if login, err = serializeLoginMessage(user, passwd); err != nil {
		return nil, err
	}
	if err = conn.writeMessage(login); err != nil {
		return nil, err
	}
	if conn.connData, err = conn.readLoginResponse(); err != nil {
		return nil, err
	}
	return conn, nil
}

// Close a connection if open. A Conn, once closed, has no further use.
// To open a new connection, use NewConnection.
func (conn *Conn) Close() error {
	var err error = nil
	if conn.tcpConn != nil {
		err = conn.tcpConn.Close()
	}
	conn.tcpConn = nil
	conn.connData = nil
	return err
}

// GoString provides a default printable format for Conn.
func (conn *Conn) GoString() string {
	if conn.connData != nil {
		return fmt.Sprintf("hostId:%v, connId:%v, leaderAddr:%v buildString:%v",
			conn.connData.hostId, conn.connData.connId,
			conn.connData.leaderAddr, conn.connData.buildString)
	}
	return "uninitialized"
}

// Ping the database for liveness.
func (conn *Conn) TestConnection() bool {
	if conn.tcpConn == nil {
		return false
	}
	rsp, err := conn.Call("@Ping")
	if err != nil {
		fmt.Println(err)
		return false
	}
	return rsp.Status() == SUCCESS
}

func (conn *Conn) Query(procedure string, params ...interface{}) (*Query, error) {
	var call bytes.Buffer
	var err error

	if conn.tcpConn == nil {
		return nil, fmt.Errorf("Can not call procedure on closed Conn.")
	}
	// Use 0 for handle; it's not necessary in pure sync client.
	conn.connData.mu.Lock()
	defer conn.connData.mu.Unlock()
	conn.connData.connCount++
	if call, err = serializeCall(procedure, conn.connData.connCount, params); err != nil {
		return nil, err
	}
	if err := conn.writeMessage(call); err != nil {
		return nil, err
	}
	size, err := conn.readMessageHdr()
	if err != nil {
		return nil, err
	}
	resp, err := conn.deserializeResponseMetadata()
	return &Query{
		size,
		resp,
		conn.tcpConn,
	}, nil
}

// Call invokes the procedure 'procedure' with parameter values 'params'
// and returns a pointer to the received Response.
func (conn *Conn) Call(procedure string, params ...interface{}) (*Response, error) {
	var call bytes.Buffer
	var err error

	if conn.tcpConn == nil {
		return nil, fmt.Errorf("Can not call procedure on closed Conn.")
	}
	if call, err = serializeCall(procedure, conn.connData.connCount, params); err != nil {
		return nil, err
	}
	if err := conn.writeMessage(call); err != nil {
		return nil, err
	}
	if _, err = conn.readMessageHdr(); err != nil {
		return nil, err
	}
	return conn.deserializeCallResponse()
}

// Response is a stored procedure result.
type Response struct {
	clientData      int64
	fieldsPresent   uint8
	status          int8
	statusString    string
	appStatus       int8
	appStatusString string
	clusterLatency  int32
	exceptionLength int32
	exceptionBytes  []byte
	resultCount     int16
	tables          []Table
}

type Query struct {
	size 	int32
	resp 	*Response
	reader  *net.TCPConn
}

type Iterator struct {
	query 	*Query
	m 		sync.Mutex
	Err 	error
	tables 	int16
	table 	Table
	buffer 	chan *bytes.Buffer
	bufSize int
}

func (q *Query) Iter() *Iterator {
	t, err := deserializeTable(q.reader)
	if  err != nil {
		log.Println(err)
		return nil
	}
	it := &Iterator{
		query: 	q,
		Err: 	nil,
		tables: q.resp.resultCount,
		table: 	t,
		bufSize:10,
	}
	it.buffer = make(chan *bytes.Buffer, it.bufSize)
	go it.fillBuffer()
	return it
}

func (iter *Iterator) fillBuffer() {
	for {
		if iter.table.rowCount == 0 {
			iter.tables--
			if iter.tables == 0 {
				iter.query.reader.Close()
				close(iter.buffer)
				return
			}
			var err error
			if iter.table, err = deserializeTable(iter.query.reader); err != nil {
				iter.Err = err
				continue
			}
		}

		// each row has a 4 byte length
		n, err := readInt(iter.query.reader)
		if err != nil {
			iter.Err = err
		} else if n <= 0 {
			iter.Err = fmt.Errorf("No more row data.")
			continue
		}
		iter.table.rowCount--
		iter.table.byteCount = iter.table.byteCount - int64(n)

		data := make([]byte, n)
		length, err := io.ReadAtLeast(iter.query.reader, data, int(n))
		if int32(length) != n {
			fmt.Println("WTF!?")
			fmt.Println("Got", length, "Expected", n)
		}
		if err != nil {
			iter.Err = err
		}
		iter.buffer <- bytes.NewBuffer(data)
	}
}

func (iter *Iterator) Next(row interface{}) bool {
	buf, ok := <- iter.buffer
	if !ok {
		return false
	}
	err := iter.table.next(row, buf)	
	if err != nil {
		iter.Err = err
	}
	return true
}

// Response status codes
type Status int

const (
	SUCCESS            Status = 1
	USER_ABORT         Status = -1
	GRACEFUL_FAILURE   Status = -2
	UNEXPECTED_FAILURE Status = -3
	CONNECTION_LOST    Status = -4
)

func (s Status) String() string {
	if s == SUCCESS {
		return "SUCCESS"
	} else if s == USER_ABORT {
		return "USER ABORT"
	} else if s == GRACEFUL_FAILURE {
		return "GRACEFUL FAILURE"
	} else if s == UNEXPECTED_FAILURE {
		return "UNEXPECTED FAILURE"
	} else if s == CONNECTION_LOST {
		return "CONNECTION LOST"
	} else {
		panic(fmt.Sprintf("Invalid status code: %d", int(s)))
	}
	return "unreachable"
}

func (rsp *Response) Status() Status {
	return Status(rsp.status)
}

func (rsp *Response) StatusString() string {
	return rsp.statusString
}

func (rsp *Response) AppStatus() int {
	return int(rsp.appStatus)
}

func (rsp *Response) AppStatusString() string {
	return rsp.appStatusString
}

func (rsp *Response) ClusterLatency() int {
	return int(rsp.clusterLatency)
}

func (rsp *Response) ResultSets() []Table {
	return rsp.tables
}

func (rsp *Response) Table(offset int) *Table {
	return &rsp.tables[offset]
}

func (rsp *Response) GoString() string {
	return fmt.Sprintf("Response: clientData:%v, status:%v, statusString:%v, "+
		"clusterLatency: %v, appStatus: %v, appStatusString: %v\n",
		rsp.clientData, rsp.status, rsp.statusString,
		rsp.clusterLatency, rsp.appStatus, rsp.appStatusString)
}

// Table represents a single result set for a stored procedure invocation.
type Table struct {
	statusCode  int8
	columnCount int16
	columnTypes []int8
	columnNames []string
	rowCount    int32
	rows        bytes.Buffer
	byteCount 	int64
}

func (table *Table) GoString() string {
	return fmt.Sprintf("Table: statusCode: %v, columnCount: %v, "+
		"rowCount: %v\n", table.statusCode, table.columnCount,
		table.rowCount)
}

func (table *Table) StatusCode() int {
	return int(table.statusCode)
}

func (table *Table) ColumnCount() int {
	return int(table.columnCount)
}

func (table *Table) ColumnTypes() []int8 {
	rv := make([]int8, 0)
	rv = append(rv, table.columnTypes...)
	return rv
}

func (table *Table) ColumnNames() []string {
	rv := make([]string, 0)
	rv = append(rv, table.columnNames...)
	return rv
}

// Rowcount returns the number of rows returned by the server for this table.
func (table *Table) RowCount() int {
	return int(table.rowCount)
}

// Next populates v (*struct) with the values of the next row.
func (table *Table) Next(v interface{}) error {
	// each row has a 4 byte length
	n, err := readInt(&table.rows)
	if err != nil {
		return err
	} else if n <= 0 {
		return fmt.Errorf("No more row data.")

	}
	return table.next(v, &table.rows)
}

// HasNext returns true of there are additional rows to read.
func (table *Table) HasNext() bool {
	return table.rows.Len() > 0
}
