package qlog

import (
	"database/sql"
	"fmt"
	"net"
	"os"
	"path"
	"strings"
	"time"

	"github.com/GeertJohan/go.rice"
	"github.com/miekg/dns"
	"github.com/patrickmn/go-cache"
	log "github.com/sirupsen/logrus"

	"github.com/chrisruffalo/gudgeon/config"
	"github.com/chrisruffalo/gudgeon/db"
	"github.com/chrisruffalo/gudgeon/resolver"
	"github.com/chrisruffalo/gudgeon/rule"
	"github.com/chrisruffalo/gudgeon/util"
)

const (
	// constant insert statement
	qlogInsertStatement = "INSERT INTO qlog (Address, ClientName, Consumer, RequestDomain, RequestType, ResponseText, Blocked, Match, MatchList, MatchRule, Cached, Created) VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
)

// lit of valid sort names (lower case for ease of use with util.StringIn)
var validSorts = []string{"address", "connectiontype", "requestdomain", "requesttype", "blocked", "blockedlist", "blockedrule", "created"}

// allows a dependency injection-way of defining a reverse lookup function, takes a string address (should be an IP) and returns a string that contains the domain name result
type ReverseLookupFunction = func(addres string) string

// info passed over channel and stored in database
// and that is recovered via the Query method
type LogInfo struct {
	// client address
	Address string

	// hold the information but aren't serialized
	Request        *dns.Msg                   `json:"-"`
	Response       *dns.Msg                   `json:"-"`
	Result         *resolver.ResolutionResult `json:"-"`
	RequestContext *resolver.RequestContext   `json:"-"`

	// generated/calculated values
	Consumer       string
	ClientName     string
	ConnectionType string
	RequestDomain  string
	RequestType    string
	ResponseText   string
	// hard consumer blocked
	Blocked        bool
	// matching
	Match          rule.Match
	MatchList      string
	MatchRule      string
	// cached in resolver cache store
	Cached         bool
	// when this log record was created
	// todo: add when it was received and when it was completed
	//       through the context so we can compute a delta
	Created        time.Time
}

// the type that is used to make queries against the
// query log (should be used by the web interface to
// find queries)
type QueryLogQuery struct {
	// query on fields
	Address        string
	ClientName     string
	ConnectionType string
	RequestDomain  string
	RequestType    string
	ResponseText   string
	Blocked        *bool 
	Cached         *bool
	Match          *rule.Match
	// query on created time
	After  *time.Time
	Before *time.Time
	// query limits for paging
	Skip  int
	Limit int
	// query sort
	SortBy    string
	Direction string
}

// store database location
type qlog struct {
	rlookup   ReverseLookupFunction
	cache     *cache.Cache
	mdnsCache *cache.Cache

	fileLogger *log.Logger
	stdLogger  *log.Logger

	store *sql.DB
	tx    *sql.Tx
	pstmt *sql.Stmt

	qlConf      *config.GudgeonQueryLog
	logInfoChan chan *LogInfo
	doneChan    chan bool
}

// public interface
type QLog interface {
	Query(query *QueryLogQuery) ([]*LogInfo, uint64)
	Log(address *net.IP, request *dns.Msg, response *dns.Msg, rCon *resolver.RequestContext, result *resolver.ResolutionResult)
	Stop()
}

func NewWithReverseLookup(conf *config.GudgeonConfig, rlookup ReverseLookupFunction) (QLog, error) {
	qlConf := conf.QueryLog
	if qlConf == nil || !*(qlConf.Enabled) {
		return nil, nil
	}

	// create new empty qlog
	qlog := &qlog{}
	qlog.qlConf = qlConf
	if qlog != nil && rlookup != nil {
		qlog.rlookup = rlookup
	}
	// create reverse lookup cache with given ttl and given reap interval
	if *qlConf.ReverseLookup {
		qlog.cache = cache.New(5*time.Minute, 10*time.Minute)
		if *qlConf.MdnsLookup {
			qlog.mdnsCache = cache.New(cache.NoExpiration, cache.NoExpiration)

			// create background channel for listening
			msgChan := make(chan *dns.Msg)
			go MulticastMdnsListen(msgChan)
			go CacheMulticastMessages(qlog.mdnsCache, msgChan)
			// and create backoff for timer for multicast query
			go func() {
				// create and start timer
				duration := 1 * time.Second
				mdnsQueryTimer := time.NewTimer(duration)

				// wait for time and do actions
				for _ = range mdnsQueryTimer.C {
					// make query
					MulticastMdnsQuery()

					// extend timer, should be exponential backoff but this is close enough
					duration = duration * 10
					if duration > time.Hour {
						duration = time.Hour
					}
					mdnsQueryTimer.Reset(duration)
				}
			}()
		}
	}

	// create distinct loggers for query output
	if qlConf.File != "" {
		// create destination and writer
		dirpart := path.Dir(qlConf.File)
		if _, err := os.Stat(dirpart); os.IsNotExist(err) {
			os.MkdirAll(dirpart, os.ModePerm)
		}

		// attempt to open file
		w, err := os.OpenFile(qlConf.File, os.O_RDWR|os.O_CREATE|os.O_APPEND, os.ModePerm)
		if err != nil {
			log.Errorf("While opening query log file: %s", err)
		} else {
			log.Infof("Logging queries to file: %s", qlConf.File)
			qlog.fileLogger = log.New()
			qlog.fileLogger.SetOutput(w)
			qlog.fileLogger.SetLevel(log.InfoLevel)
			qlog.fileLogger.SetFormatter(&log.JSONFormatter{})
		}
	}

	if *(qlConf.Stdout) {
		log.Info("Logging queries to stdout")
		qlog.stdLogger = log.New()
		qlog.stdLogger.SetOutput(os.Stdout)
		qlog.stdLogger.SetLevel(log.InfoLevel)
		qlog.stdLogger.SetFormatter(&log.TextFormatter{
			FullTimestamp:   true,
			TimestampFormat: "2006-01-02 15:04:05",
		})
	}

	// create log channel for queuing up batches
	qlog.logInfoChan = make(chan *LogInfo, qlConf.QueueSize)
	qlog.doneChan = make(chan bool)
	go qlog.logWorker()

	// only build DB if persistence is enabled
	if *(qlog.qlConf.Persist) {
		// get path to long-standing data ({home}/'data') and make sure it exists
		dataDir := conf.DataRoot()
		if _, err := os.Stat(dataDir); os.IsNotExist(err) {
			os.MkdirAll(dataDir, os.ModePerm)
		}

		// open db
		dbDir := path.Join(dataDir, "query_log")
		// create directory
		if _, err := os.Stat(dbDir); os.IsNotExist(err) {
			os.MkdirAll(dbDir, os.ModePerm)
		}

		dbPath := path.Join(dbDir, "qlog.db")

		// get migrations
		migrationsBox := rice.MustFindBox("qlog-migrations")

		// open database
		var err error
		qlog.store, err = db.OpenAndMigrate(dbPath, "", migrationsBox)
		if err != nil {
			return nil, err
		}

		// prune entries
		qlog.prune()
	}

	return qlog, nil
}

// create a new query log according to configuration
func New(conf *config.GudgeonConfig) (QLog, error) {
	return NewWithReverseLookup(conf, nil)
}

func (qlog *qlog) prune() {
	duration, _ := util.ParseDuration(qlog.qlConf.Duration)
	_, err := qlog.store.Exec("DELETE FROM qlog WHERE Created <= ?", time.Now().Add(-1*duration))
	if err != nil {
		log.Errorf("Error pruning qlog data: %s", err)
	}
}

func (qlog *qlog) queue(info *LogInfo) {
	// only add to batch if not nil
	if info == nil {
		return
	}

	var err error

	if qlog.tx == nil {
		// close old statement
		if qlog.pstmt != nil {
			qlog.pstmt.Close()
		}

		// new transaction means the old stmt is dead
		qlog.pstmt = nil

		qlog.tx, err = qlog.store.Begin()
		if err != nil {
			log.Errorf("Could not start transaction: %s", err)
			return
		}
	}

	if qlog.pstmt == nil {
		qlog.pstmt, err = qlog.tx.Prepare(qlogInsertStatement)
		if err != nil {
			qlog.tx.Rollback()
			log.Errorf("Preparing statement: %s", err)
			qlog.tx = nil
			qlog.pstmt = nil
			return
		}
	}

	_, err = qlog.pstmt.Exec(info.Address, info.ClientName, info.Consumer, info.RequestDomain, info.RequestType, info.ResponseText, info.Blocked, info.Match, info.MatchList, info.MatchRule, info.Cached, info.Created)
	if err != nil {
		log.Errorf("Insert into qlog: %s", err)
	}
}

func (qlog *qlog) flush() {
	if qlog.tx == nil {
		return
	}

	// if statement is open, close it
	if qlog.pstmt != nil {
		qlog.pstmt.Close()
		qlog.pstmt = nil
	}

	// commit transaction
	qlog.tx.Commit()
	qlog.tx = nil
}

func (qlog *qlog) log(info *LogInfo) {
	// get values
	response := info.Response
	result := info.Result
	rCon := info.RequestContext

	// create builder
	var builder strings.Builder

	var fields log.Fields
	if qlog.fileLogger != nil {
		fields = log.Fields{}
	}

	// log result if found
	builder.WriteString("[")
	if info.ClientName != "" {
		builder.WriteString(info.ClientName)
		if qlog.fileLogger != nil {
			fields["clientName"] = info.ClientName
		}
		builder.WriteString("|")
	}
	builder.WriteString(info.Address)
	builder.WriteString("/")
	builder.WriteString(rCon.Protocol)
	builder.WriteString("|")
	builder.WriteString(info.Consumer)
	builder.WriteString("] q:[")
	builder.WriteString(info.RequestDomain)
	builder.WriteString("|")
	builder.WriteString(info.RequestType)
	builder.WriteString("]->")
	if qlog.fileLogger != nil {
		fields["address"] = info.Address
		fields["protocol"] = rCon.Protocol
		fields["consumer"] = info.Consumer
		fields["requestDomain"] = info.RequestDomain
		fields["requestType"] = info.RequestType
		fields["cached"] = false
	}

	if result != nil {
		if result.Blocked {
			builder.WriteString("BLOCKED")
		} else if result.Match == rule.MatchBlock {
			builder.WriteString("RULE BLOCKED")
			if qlog.fileLogger != nil {
				fields["match"] = result.Match
				fields["matchType"] = "BLOCKED"
			}
			if result.MatchList != nil {
				builder.WriteString("[")
				builder.WriteString(result.MatchList.CanonicalName())
				if qlog.fileLogger != nil {
					fields["matchList"] = result.MatchList.CanonicalName()
				}
				if result.MatchRule != "" {
					builder.WriteString("|")
					builder.WriteString(result.MatchRule)
					if qlog.fileLogger != nil {
						fields["matchRule"] = result.MatchRule
					}
				}
				builder.WriteString("]")
			}
		} else {
			if result.Match == rule.MatchAllow {
				if qlog.fileLogger != nil {
					fields["match"] = result.Match
					fields["matchType"] = "ALLOWED"
				}
			}
			if result.Cached {
				builder.WriteString("c:[")
				builder.WriteString(result.Resolver)
				builder.WriteString("]")
				if qlog.fileLogger != nil {
					fields["resolver"] = result.Resolver
					fields["cached"] = "true"
				}
			} else {
				builder.WriteString("r:[")
				builder.WriteString(result.Resolver)
				builder.WriteString("]")
				builder.WriteString("->")
				builder.WriteString("s:[")
				builder.WriteString(result.Source)
				builder.WriteString("]")
				if qlog.fileLogger != nil {
					fields["resolver"] = result.Resolver
					fields["source"] = result.Source
				}
			}

			builder.WriteString("->")

			if len(response.Answer) > 0 {
				answerValues := util.GetAnswerValues(response)
				if len(answerValues) > 0 {
					builder.WriteString(answerValues[0])
					if qlog.fileLogger != nil {
						fields["answer"] = answerValues[0]
					}
					if len(answerValues) > 1 {
						builder.WriteString(fmt.Sprintf(" (+%d)", len(answerValues)-1))
					}
				} else {
					builder.WriteString("(EMPTY RESPONSE)")
					if qlog.fileLogger != nil {
						fields["answer"] = "<< EMPTY >>"
					}
				}
			} else {
				builder.WriteString("(NO INFO RESPONSE)")
				if qlog.fileLogger != nil {
					fields["answer"] = "<< NONE >>"
				}
			}
		}
	} else if response.Rcode == dns.RcodeServerFailure {
		// write as error and return
		if qlog.fileLogger != nil {
			qlog.fileLogger.WithFields(fields).Error(fmt.Sprintf("SERVFAIL:[%s]", result.Message))
		}
		if qlog.stdLogger != nil {
			builder.WriteString(fmt.Sprintf("SERVFAIL:[%s]", result.Message))
			qlog.stdLogger.Error(builder.String())
		}

		return
	} else {
		builder.WriteString(fmt.Sprintf("RESPONSE[%s]", dns.RcodeToString[response.Rcode]))
	}

	// output built string
	if qlog.fileLogger != nil {
		qlog.fileLogger.WithFields(fields).Info(dns.RcodeToString[response.Rcode])
	}
	if qlog.stdLogger != nil {
		qlog.stdLogger.Info(builder.String())
	}
}

func (qlog *qlog) getReverseName(address string) string {
	if !*qlog.qlConf.ReverseLookup {
		return ""
	}

	// look in local cache for name, even if it is empty
	if value, found := qlog.cache.Get(address); found {
		if valueString, ok := value.(string); ok {
			return valueString
		}
	}

	name := ""

	// if there is a reverselookup function use it to add a reverse lookup step
	if *qlog.qlConf.ReverseLookup && qlog.rlookup != nil {
		name = qlog.rlookup(address)
		if strings.HasSuffix(name, ".") {
			name = name[:len(name)-1]
		}
	}

	// look in the mdns cache
	if *qlog.qlConf.MdnsLookup && qlog.mdnsCache != nil {
		name := ReadCachedHostname(qlog.mdnsCache, address)
		if name != "" {
			return name
		}
	}

	// if no result from rlookup then try and lookup the netbios name from the host
	if *qlog.qlConf.NetbiosLookup && "" == name {
		var err error
		name, err = util.LookupNetBIOSName(address)
		if err != nil {
			// don't really need to see these
			log.Tracef("During NETBIOS lookup: %s", err)
		}
	}

	if qlog.cache != nil {
		// store result, even empty results, to prevent continual lookups
		qlog.cache.Set(address, name, cache.DefaultExpiration)
	}

	return name
}

// this is the actual log worker that handles incoming log messages in a separate go routine
func (qlog *qlog) logWorker() {
	// create ticker from conf
	duration, err := util.ParseDuration(qlog.qlConf.BatchInterval)
	if err != nil {
		duration = 1 * time.Second
	}
	flushTimer := time.NewTimer(duration)
	defer flushTimer.Stop()
	// prune every hour (also prunes on startup)
	pruneTimer := time.NewTimer(1 * time.Hour)
	defer pruneTimer.Stop()

	// stop the timer immediately if we aren't persisting records
	if !*(qlog.qlConf.Persist) {
		flushTimer.Stop()
		pruneTimer.Stop()
	}

	// loop until...
	for {
		select {
		case info := <-qlog.logInfoChan:

			if info != nil {
				// condition the log info item in this thread
				if info.Request != nil && len(info.Request.Question) > 0 {
					info.RequestDomain = info.Request.Question[0].Name
					info.RequestType = dns.Type(info.Request.Question[0].Qtype).String()
				}

				if info.Response != nil {
					answerValues := util.GetAnswerValues(info.Response)
					if len(answerValues) > 0 {
						info.ResponseText = answerValues[0]
					}
				}

				if info.Result != nil {
					info.Consumer = info.Result.Consumer

					if info.Result.Blocked {
						info.Blocked = true
					}

					if info.Result.Cached {
						info.Cached = true
					}

					info.Match = info.Result.Match
					if info.Result.Match != rule.MatchNone {
						if info.Result.MatchList != nil {
							info.MatchList = info.Result.MatchList.CanonicalName()
						}
						info.MatchRule = info.Result.MatchRule
					}
				}

				if info.RequestContext != nil {
					info.ConnectionType = info.RequestContext.Protocol
				}

				// get reverse lookup name
				info.ClientName = qlog.getReverseName(info.Address)
			}

			// only log to
			if info != nil && ("" != qlog.qlConf.File || *(qlog.qlConf.Stdout)) {
				qlog.log(info)
			}
			// only persist if configured, which is default
			if *(qlog.qlConf.Persist) {
				qlog.queue(info)
			}
		case <-qlog.doneChan:
			// when the function is over the shutdown method waits for
			// a message back on the doneChan to know that we are done
			// shutting down
			defer func() { qlog.doneChan <- true }()
			return
		case <-flushTimer.C:
			log.Tracef("Flush timer triggered")
			qlog.flush()
			flushTimer.Reset(duration)
		case <-pruneTimer.C:
			log.Tracef("Prune timer triggered")
			qlog.prune()
			pruneTimer.Reset(1 * time.Hour)
		}
	}
}

func (qlog *qlog) Log(address *net.IP, request *dns.Msg, response *dns.Msg, rCon *resolver.RequestContext, result *resolver.ResolutionResult) {
	// create message for sending to various endpoints
	msg := new(LogInfo)
	msg.Address = address.String()
	msg.Request = request
	msg.Response = response
	msg.Result = result
	msg.RequestContext = rCon
	msg.Created = time.Now()
	// put on channel
	qlog.logInfoChan <- msg
}

func (qlog *qlog) Query(query *QueryLogQuery) ([]*LogInfo, uint64) {
	// select entries from qlog
	selectStmt := "SELECT Address, ClientName, Consumer, RequestDomain, RequestType, ResponseText, Blocked, Match, MatchList, MatchRule, Cached, Created FROM qlog"
	countStmt := "SELECT COUNT(*) FROM qlog"

	// so we can dynamically build the where clause
	orClauses := []string{"1 = 1"}
	whereClauses := []string{"1 = 1"}
	orValues := make([]interface{}, 0)
	whereValues := make([]interface{}, 0)

	// result holding
	var rows *sql.Rows
	var err error

	// or clause
	if "" != query.Address {
		orClauses = append(orClauses, "Address like ?")
		orValues = append(orValues, "%"+query.Address+"%")
	}

	if "" != query.ClientName {
		orClauses = append(orClauses, "ClientName like ?")
		orValues = append(orValues, "%"+query.ClientName+"%")
	}

	if "" != query.RequestDomain {
		orClauses = append(orClauses, "RequestDomain like ?")
		orValues = append(orValues, "%"+query.RequestDomain+"%")
	}

	if "" != query.ResponseText {
		orClauses = append(orClauses, "ResponseText like ?")
		orValues = append(orValues, "%"+query.ResponseText+"%")
	}

	if query.Blocked != nil {
		whereClauses = append(whereClauses, "Blocked = ?")
		whereValues = append(whereValues, query.Blocked)
	}

	if query.Match != nil {
		whereClauses = append(whereClauses, "Match = ?")
		whereValues = append(whereValues, query.Match)
	}

	if query.Cached != nil {
		whereClauses = append(whereClauses, "Cached = ?")
		whereValues = append(whereValues, query.Cached)
	}

	if query.After != nil {
		whereClauses = append(whereClauses, "Created > ?")
		whereValues = append(whereValues, query.After)
	}

	if query.Before != nil {
		whereClauses = append(whereClauses, "Created < ?")
		whereValues = append(whereValues, query.Before)
	}

	// finalize query part
	if len(whereClauses) > 0 || len(orClauses) > 0 {
		if len(orClauses) > 1 {
			orClauses = orClauses[1:]
		}
		if len(whereClauses) > 1 {
			whereClauses = whereClauses[1:]
		}

		clauses := strings.Join([]string{"(" + strings.Join(orClauses, " OR ") + ")", strings.Join(whereClauses, " AND ")}, " AND ")
		selectStmt = selectStmt + " WHERE " + clauses
		// copy current select statement to use for length query if needed
		countStmt = countStmt + " WHERE " + clauses
	}

	// add or/and values together
	whereValues = append(orValues, whereValues...)

	// sort and sort direction
	sortBy := "created"
	direction := strings.ToUpper(query.Direction)
	if !util.StringIn(direction, []string{"DESC", "ASC"}) {
		direction = ""
	}
	if "" != query.SortBy && util.StringIn(strings.ToLower(query.SortBy), validSorts) {
		sortBy = query.SortBy
	}
	if "created" == strings.ToLower(sortBy) && "" == direction {
		direction = "DESC"
	} else if "" == direction {
		direction = "ASC"
	}

	// add sort
	selectStmt = selectStmt + fmt.Sprintf(" ORDER BY %s %s", sortBy, direction)

	// default length of query is 0
	resultLen := uint64(0)
	checkLen := false

	// set limits
	if query.Limit > 0 {
		selectStmt = selectStmt + fmt.Sprintf(" LIMIT %d", query.Limit)
		checkLen = true
	}
	if query.Skip > 0 {
		selectStmt = selectStmt + fmt.Sprintf(" OFFSET %d", query.Skip)
		checkLen = true
	}

	//log.Infof("Query: %s", selectStmt)
	//log.Infof("Values: %v", whereValues)

	// get query length by itself without offsets and limits
	// but based on the same query
	if checkLen {
		err := qlog.store.QueryRow(countStmt, whereValues...).Scan(&resultLen)
		if err != nil {
			log.Errorf("Could not get log item count: %s", err)
			checkLen = false
		}
	}

	// make query
	rows, err = qlog.store.Query(selectStmt, whereValues...)
	if err != nil {
		log.Errorf("Query log query failed: %s", err)
		return []*LogInfo{}, 0
	}
	defer rows.Close()
	// if rows is nil return empty array
	if rows == nil {
		return []*LogInfo{}, 0
	}

	// otherwise create an array of the required size
	results := make([]*LogInfo, 0, resultLen)

	// scan each row and get results
	var info *LogInfo
	for rows.Next() {
		info = &LogInfo{}
		err = rows.Scan(&info.Address, &info.ClientName, &info.Consumer, &info.RequestDomain, &info.RequestType, &info.ResponseText, &info.Blocked, &info.Match, &info.MatchList, &info.MatchRule, &info.Cached, &info.Created)
		if err != nil {
			log.Errorf("Scanning qlog results: %s", err)
			continue
		}
		results = append(results, info)
	}

	if !checkLen {
		resultLen = uint64(len(results))
	}

	return results, resultLen
}

func (qlog *qlog) Stop() {
	// be done
	qlog.doneChan <- true
	<-qlog.doneChan

	// close channels
	close(qlog.doneChan)
	close(qlog.logInfoChan)

	// flush pending records
	qlog.flush()
	// prune old records
	qlog.prune()
	// close db
	qlog.store.Close()
}
