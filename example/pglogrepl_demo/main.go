package main

import (
	"context"
	"log"
	"os"
	"time"
	"fmt"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"

	"net/http"
	_ "net/http/pprof"
)

func createReplicationOrigin(conn *pgx.Conn, name string) error {
	// create origin if not exists
	q := `SELECT * FROM pg_replication_origin WHERE roname = $1`
	row := conn.QueryRow(context.Background(), q, name)
	var originID uint64
	err := row.Scan(&originID)
	if err == pgx.ErrNoRows {
		q := `SELECT pg_replication_origin_create($1)`
		_, err = conn.Exec(context.Background(), q, name)
		if err != nil {
			return err
		}
	}

	q = `SELECT pg_replication_origin_session_setup($1)`
	_, err = conn.Exec(context.Background(), q, name)
	if err != nil {
		return err
	}
	return nil
}

func main() {
	go func() {
        log.Println(http.ListenAndServe("localhost:6060", nil))
  }()

	//	const outputPlugin = "test_decoding"
	const outputPlugin = "pgoutput"
	//const outputPlugin = "wal2json"
	targetConn, err := pgx.Connect(context.Background(), os.Getenv("TARGET"))
	if err != nil {
		log.Fatalln("failed to connect to PostgreSQL server:", err)
	}

	err = createReplicationOrigin(targetConn, "pglogrepl_demo")
	if err != nil {
		log.Fatalln("failed to create replication origin:", err)
	}

	conn, err := pgconn.Connect(context.Background(), os.Getenv("SOURCE"))
	if err != nil {
		log.Fatalln("failed to connect to PostgreSQL server:", err)
	}
	defer conn.Close(context.Background())

	// result := conn.Exec(context.Background(), "DROP PUBLICATION IF EXISTS pglogrepl_demo;")
	// _, err = result.ReadAll()
	// if err != nil {
	// 	log.Fatalln("drop publication if exists error", err)
	// }

	// result = conn.Exec(context.Background(), "CREATE PUBLICATION pglogrepl_demo FOR ALL TABLES;")
	// _, err = result.ReadAll()
	// if err != nil {
	// 	log.Fatalln("create publication error", err)
	// }
	// log.Println("create publication pglogrepl_demo")

	var pluginArguments []string
	var v2 bool
	if outputPlugin == "pgoutput" {
		// streaming of large transactions is available since PG 14 (protocol version 2)
		// we also need to set 'streaming' to 'true'
		pluginArguments = []string{
			"proto_version '2'",
			"publication_names 'pglogrepl_demo'",
			"messages 'true'",
			"streaming 'true'",
		}
		v2 = true
		// uncomment for v1
		// pluginArguments = []string{
		//	"proto_version '1'",
		//	"publication_names 'pglogrepl_demo'",
		//	"messages 'true'",
		// }
	} else if outputPlugin == "wal2json" {
		pluginArguments = []string{"\"pretty-print\" 'true'"}
	}

	sysident, err := pglogrepl.IdentifySystem(context.Background(), conn)
	if err != nil {
		log.Fatalln("IdentifySystem failed:", err)
	}
	log.Println("SystemID:", sysident.SystemID, "Timeline:", sysident.Timeline, "XLogPos:", sysident.XLogPos, "DBName:", sysident.DBName)

	slotName := "pglogrepl_demo"

	_, err = pglogrepl.CreateReplicationSlot(context.Background(), conn, slotName, outputPlugin, pglogrepl.CreateReplicationSlotOptions{Temporary: true})
	if err != nil {
		log.Fatalln("CreateReplicationSlot failed:", err)
	}
	log.Println("Created temporary replication slot:", slotName)

	err = pglogrepl.StartReplication(context.Background(), conn, slotName, sysident.XLogPos, pglogrepl.StartReplicationOptions{PluginArgs: pluginArguments})
	if err != nil {
		log.Fatalln("StartReplication failed:", err)
	}
	log.Println("Logical replication started on slot", slotName)

	clientXLogPos := sysident.XLogPos
	standbyMessageTimeout := time.Second * 10
	nextStandbyMessageDeadline := time.Now().Add(standbyMessageTimeout)
	relations := map[uint32]*pglogrepl.RelationMessage{}
	relationsV2 := map[uint32]*pglogrepl.RelationMessageV2{}
	typeMap := pgtype.NewMap()

	// whenever we get StreamStartMessage we set inStream to true and then pass it to DecodeV2 function
	// on StreamStopMessage we set it back to false
	inStream := false

	applyCtx := applyContext{conn: targetConn, lastCommitTime: time.Now(), timer: time.NewTimer(2 * time.Second)}

	walDataCh := make(chan []byte, 1024)
	go func() {
		for {
			select {
			case <-applyCtx.timer.C:
				applyCtx.flush(context.Background())
			case walData := <-walDataCh:
				if v2 {
					processV2(walData, relationsV2, typeMap, &inStream, &applyCtx)
				} else {
					processV1(walData, relations, typeMap, &applyCtx)
				}
			}
		}
	}()

	for {
		if time.Now().After(nextStandbyMessageDeadline) {
			err = pglogrepl.SendStandbyStatusUpdate(context.Background(), conn, pglogrepl.StandbyStatusUpdate{WALWritePosition: clientXLogPos})
			if err != nil {
				log.Fatalln("SendStandbyStatusUpdate failed:", err)
			}
			log.Printf("Sent Standby status message at %s\n", clientXLogPos.String())
			nextStandbyMessageDeadline = time.Now().Add(standbyMessageTimeout)
		}

		ctx, cancel := context.WithDeadline(context.Background(), nextStandbyMessageDeadline)
		rawMsg, err := conn.ReceiveMessage(ctx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) {
				continue
			}
			log.Fatalln("ReceiveMessage failed:", err)
		}

		if errMsg, ok := rawMsg.(*pgproto3.ErrorResponse); ok {
			log.Fatalf("received Postgres WAL error: %+v", errMsg)
		}

		msg, ok := rawMsg.(*pgproto3.CopyData)
		if !ok {
			log.Printf("Received unexpected message: %T\n", rawMsg)
			continue
		}

		switch msg.Data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			pkm, err := pglogrepl.ParsePrimaryKeepaliveMessage(msg.Data[1:])
			if err != nil {
				log.Fatalln("ParsePrimaryKeepaliveMessage failed:", err)
			}
			if pkm.ServerWALEnd > clientXLogPos {
				clientXLogPos = pkm.ServerWALEnd
			}
			if pkm.ReplyRequested {
				nextStandbyMessageDeadline = time.Time{}
			}

		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(msg.Data[1:])
			if err != nil {
				log.Fatalln("ParseXLogData failed:", err)
			}

			if outputPlugin == "wal2json" {
				log.Printf("wal2json data: %s\n", string(xld.WALData))
			} else {
				walDataCp := make([]byte, len(xld.WALData))
				copy(walDataCp, xld.WALData)
				walDataCh <- walDataCp
			}

			if xld.WALStart > clientXLogPos {
				clientXLogPos = xld.WALStart
			}
		}
	}
}

type applyContext struct {
	conn *pgx.Conn
	tx   pgx.Tx
	batch pgx.Batch
	lastCommitTime time.Time
	commitLSN pglogrepl.LSN
	commitTime time.Time
	txnInProgress bool
	timer *time.Timer
}

func (a *applyContext) queue(q string, args ...interface{}) {
	a.batch.Queue(q, args...)
}

func (a *applyContext) begin() {
	a.txnInProgress = true
	a.timer.Stop()
}

func (a *applyContext) commit(commitLSN pglogrepl.LSN, commitTime time.Time) {
	a.commitLSN = commitLSN
	a.commitTime = commitTime
	a.txnInProgress = false
	if time.Since(a.lastCommitTime) > 2 * time.Second {
		a.flush(context.Background())
	} else {
		a.timer.Reset(2 * time.Second)
	}
}

func (a *applyContext) flush(ctx context.Context) {
	if a.batch.Len() == 0 {
		return
	}
	q := `select pg_replication_origin_xact_setup($1, $2)`
	a.batch.Queue(q, a.commitLSN, a.commitTime)
	before := time.Now()
	err := a.conn.SendBatch(ctx, &a.batch).Close()
	if err != nil {
		log.Fatalf("failed to apply batch: %v", err)
	}
	log.Printf("commit took %v queue %d", time.Since(before), a.batch.Len())
	a.batch = pgx.Batch{}
	a.lastCommitTime = time.Now()
}

func processV2(walData []byte, relations map[uint32]*pglogrepl.RelationMessageV2, typeMap *pgtype.Map, inStream *bool, applyCtx *applyContext) {
	logicalMsg, err := pglogrepl.ParseV2(walData, *inStream)
	if err != nil {
		log.Fatalf("Parse logical replication message: %s", err)
	}
	switch logicalMsg := logicalMsg.(type) {
	case *pglogrepl.RelationMessageV2:
		relations[logicalMsg.RelationID] = logicalMsg

	case *pglogrepl.BeginMessage:
		// *tx, err = targetConn.Begin(context.Background())
		// if err != nil {
		// 	log.Fatalf("failed to start transaction: %v", err)
		// }
		// Indicates the beginning of a group of changes in a transaction. This is only sent for committed transactions. You won't get any events from rolled back transactions.

		applyCtx.begin()
	case *pglogrepl.CommitMessage:
		// err := (*tx).Commit(context.Background())
		// if err != nil {
		// 	log.Fatalf("failed to commit transaction: %v", err)
		// }
		applyCtx.commit(logicalMsg.CommitLSN, logicalMsg.CommitTime)

	case *pglogrepl.InsertMessageV2:
		rel, ok := relations[logicalMsg.RelationID]
		if !ok {
			log.Fatalf("unknown relation ID %d", logicalMsg.RelationID)
		}
		query := fmt.Sprintf("INSERT INTO %s(", pgx.Identifier{rel.Namespace, rel.RelationName}.Sanitize())

		vals := []interface{}{}
		for idx, col := range logicalMsg.Tuple.Columns {
			colName := pgx.Identifier{rel.Columns[idx].Name}.Sanitize()

			if idx == 0 {
				query += colName
			} else {
				query += ", " + colName
			}

			switch col.DataType {
			case 'n': // null
				vals = append(vals, nil)
			case 'u': // unchanged toast
				// This TOAST value was not changed. TOAST values are not stored in the tuple, and logical replication doesn't want to spend a disk read to fetch its value for you.
			case 't': //text
				val, err := decodeTextColumnData(typeMap, col.Data, rel.Columns[idx].DataType)
				if err != nil {
					log.Fatalln("error decoding column data:", err)
				}
				vals = append(vals, val)
			}
		}
		query += ") overriding system value VALUES("
		for idx := range logicalMsg.Tuple.Columns {
			if idx == 0 {
				query += fmt.Sprintf("$%d", idx+1)
			} else {
				query += fmt.Sprintf(", $%d", idx+1)
			}
		}
		query += ")"
		// _, err := (*tx).Exec(context.Background(), query, vals...)
		// if err != nil {
		// 	log.Fatalf("failed to insert into %s.%s: %v", rel.Namespace, rel.RelationName, err)
		// }
		applyCtx.queue(query, vals...)

	case *pglogrepl.UpdateMessageV2:
		log.Printf("update for xid %d\n", logicalMsg.Xid)
		// ...
	case *pglogrepl.DeleteMessageV2:
		log.Printf("delete for xid %d\n", logicalMsg.Xid)
		// ...
	case *pglogrepl.TruncateMessageV2:
		log.Printf("truncate for xid %d\n", logicalMsg.Xid)
		// ...

	case *pglogrepl.TypeMessageV2:
	case *pglogrepl.OriginMessage:

	case *pglogrepl.LogicalDecodingMessageV2:
		log.Printf("Logical decoding message: %q, %q, %d", logicalMsg.Prefix, logicalMsg.Content, logicalMsg.Xid)

	case *pglogrepl.StreamStartMessageV2:
		*inStream = true
		log.Printf("Stream start message: xid %d, first segment? %d", logicalMsg.Xid, logicalMsg.FirstSegment)
	case *pglogrepl.StreamStopMessageV2:
		*inStream = false
		log.Printf("Stream stop message")
	case *pglogrepl.StreamCommitMessageV2:
		log.Printf("Stream commit message: xid %d", logicalMsg.Xid)
	case *pglogrepl.StreamAbortMessageV2:
		log.Printf("Stream abort message: xid %d", logicalMsg.Xid)
	default:
		log.Printf("Unknown message type in pgoutput stream: %T", logicalMsg)
	}
}

func processV1(walData []byte, relations map[uint32]*pglogrepl.RelationMessage, typeMap *pgtype.Map, applyCtx *applyContext) {
	logicalMsg, err := pglogrepl.Parse(walData)
	if err != nil {
		log.Fatalf("Parse logical replication message: %s", err)
	}
	log.Printf("Receive a logical replication message: %s", logicalMsg.Type())
	switch logicalMsg := logicalMsg.(type) {
	case *pglogrepl.RelationMessage:
		relations[logicalMsg.RelationID] = logicalMsg

	case *pglogrepl.BeginMessage:
		// Indicates the beginning of a group of changes in a transaction. This is only sent for committed transactions. You won't get any events from rolled back transactions.

	case *pglogrepl.CommitMessage:

	case *pglogrepl.InsertMessage:
		rel, ok := relations[logicalMsg.RelationID]
		if !ok {
			log.Fatalf("unknown relation ID %d", logicalMsg.RelationID)
		}
		values := map[string]interface{}{}
		for idx, col := range logicalMsg.Tuple.Columns {
			colName := rel.Columns[idx].Name
			switch col.DataType {
			case 'n': // null
				values[colName] = nil
			case 'u': // unchanged toast
				// This TOAST value was not changed. TOAST values are not stored in the tuple, and logical replication doesn't want to spend a disk read to fetch its value for you.
			case 't': //text
				val, err := decodeTextColumnData(typeMap, col.Data, rel.Columns[idx].DataType)
				if err != nil {
					log.Fatalln("error decoding column data:", err)
				}
				values[colName] = val
			}
		}
		log.Printf("INSERT INTO %s.%s: %v", rel.Namespace, rel.RelationName, values)

	case *pglogrepl.UpdateMessage:
		// ...
	case *pglogrepl.DeleteMessage:
		// ...
	case *pglogrepl.TruncateMessage:
		// ...

	case *pglogrepl.TypeMessage:
	case *pglogrepl.OriginMessage:

	case *pglogrepl.LogicalDecodingMessage:
		log.Printf("Logical decoding message: %q, %q", logicalMsg.Prefix, logicalMsg.Content)

	case *pglogrepl.StreamStartMessageV2:
		log.Printf("Stream start message: xid %d, first segment? %d", logicalMsg.Xid, logicalMsg.FirstSegment)
	case *pglogrepl.StreamStopMessageV2:
		log.Printf("Stream stop message")
	case *pglogrepl.StreamCommitMessageV2:
		log.Printf("Stream commit message: xid %d", logicalMsg.Xid)
	case *pglogrepl.StreamAbortMessageV2:
		log.Printf("Stream abort message: xid %d", logicalMsg.Xid)
	default:
		log.Printf("Unknown message type in pgoutput stream: %T", logicalMsg)
	}
}

func decodeTextColumnData(mi *pgtype.Map, data []byte, dataType uint32) (interface{}, error) {
	if dt, ok := mi.TypeForOID(dataType); ok {
		return dt.Codec.DecodeValue(mi, dataType, pgtype.TextFormatCode, data)
	}
	return string(data), nil
}
