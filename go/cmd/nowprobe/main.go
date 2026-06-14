package main
import ("fmt";"os";"github.com/sobs/sobs/internal/store")
func main(){
 db,e0:=store.Open(os.Args[1]); if e0!=nil { fmt.Println("open",e0); return }
 q1:="SELECT count() AS c FROM (SELECT if(LogAttributes['sessionId'] != '', LogAttributes['sessionId'], if(LogAttributes['session.id'] != '', LogAttributes['session.id'], concat('anon:', substring(lower(hex(MD5(concat(toString(Timestamp), '|', Body)))), 1, 16)))) AS session_key FROM hyperdx_sessions GROUP BY session_key)"
 r1,e1:=db.Execute(q1); fmt.Printf("count: err=%v rows=%v\n", e1, r1.Rows)
 q2:="SELECT toString(mb) AS mb, cnt FROM (SELECT toStartOfMinute(Timestamp) AS mb, count() AS cnt FROM hyperdx_sessions WHERE EventName IN ('error','unhandledrejection') AND Timestamp >= now() - INTERVAL 180 MINUTE GROUP BY mb) ORDER BY mb WITH FILL FROM toStartOfMinute(now() - INTERVAL 180 MINUTE) TO toStartOfMinute(now()) STEP toIntervalMinute(1)"
 r2,e2:=db.Execute(q2); fmt.Printf("sparkline: err=%v nrows=%d\n", e2, len(r2.Rows))
}
