package vlstorage

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/flagutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fs"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/httpserver"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promauth"
	"github.com/VictoriaMetrics/metrics"

	"github.com/VictoriaMetrics/VictoriaLogs/app/vlstorage/netinsert"
	"github.com/VictoriaMetrics/VictoriaLogs/app/vlstorage/netselect"
	"github.com/VictoriaMetrics/VictoriaLogs/lib/logstorage"
)

var (
	retentionPeriod = flagutil.NewRetentionDuration("retentionPeriod", "7d", "Log entries with timestamps older than now-retentionPeriod are automatically deleted; "+
		"log entries with timestamps outside the retention are also rejected during data ingestion; the minimum supported retention is 1d (one day); "+
		"see https://docs.victoriametrics.com/victorialogs/#retention ; see also -retention.maxDiskSpaceUsageBytes and -retention.maxDiskUsagePercent")
	maxDiskSpaceUsageBytes = flagutil.NewBytes("retention.maxDiskSpaceUsageBytes", 0, "The maximum disk space usage at -storageDataPath before older per-day "+
		"partitions are automatically dropped; see https://docs.victoriametrics.com/victorialogs/#retention-by-disk-space-usage ; see also -retentionPeriod")
	maxDiskUsagePercent = flag.Int("retention.maxDiskUsagePercent", 0, "The maximum allowed disk usage percentage (1-100) for the filesystem that contains -storageDataPath before older per-day partitions are automatically dropped; mutually exclusive with -retention.maxDiskSpaceUsageBytes; see https://docs.victoriametrics.com/victorialogs/#retention-by-disk-space-usage-percent")
	futureRetention     = flagutil.NewRetentionDuration("futureRetention", "2d", "Log entries with timestamps bigger than now+futureRetention are rejected during data ingestion; "+
		"see https://docs.victoriametrics.com/victorialogs/#retention")
	storageDataPath = flag.String("storageDataPath", "victoria-logs-data", "Path to directory where to store VictoriaLogs data; "+
		"see https://docs.victoriametrics.com/victorialogs/#storage")
	inmemoryDataFlushInterval = flag.Duration("inmemoryDataFlushInterval", 5*time.Second, "The interval for guaranteed saving of in-memory data to disk. "+
		"The saved data survives unclean shutdowns such as OOM crash, hardware reset, SIGKILL, etc. "+
		"Bigger intervals may help increase the lifetime of flash storage with limited write cycles (e.g. Raspberry PI). "+
		"Smaller intervals increase disk IO load. Minimum supported value is 1s")
	logNewStreams = flag.Bool("logNewStreams", false, "Whether to log creation of new streams; this can be useful for debugging of high cardinality issues with log streams; "+
		"see https://docs.victoriametrics.com/victorialogs/keyconcepts/#stream-fields ; see also -logIngestedRows")
	logIngestedRows = flag.Bool("logIngestedRows", false, "Whether to log all the ingested log entries; this can be useful for debugging of data ingestion; "+
		"see https://docs.victoriametrics.com/victorialogs/data-ingestion/ ; see also -logNewStreams")
	minFreeDiskSpaceBytes = flagutil.NewBytes("storage.minFreeDiskSpaceBytes", 10e6, "The minimum free disk space at -storageDataPath after which "+
		"the storage stops accepting new data")

	forceMergeAuthKey = flagutil.NewPassword("forceMergeAuthKey", "authKey, which must be passed in query string to /internal/force_merge . It overrides -httpAuth.* . "+
		"See https://docs.victoriametrics.com/victorialogs/#forced-merge")
	forceFlushAuthKey = flagutil.NewPassword("forceFlushAuthKey", "authKey, which must be passed in query string to /internal/force_flush . It overrides -httpAuth.* . "+
		"See https://docs.victoriametrics.com/victorialogs/#forced-flush")

	partitionManageAuthKey = flagutil.NewPassword("partitionManageAuthKey", "authKey, which must be passed in query string to /internal/partition/* . It overrides -httpAuth.* . "+
		"See https://docs.victoriametrics.com/victorialogs/#partitions-lifecycle")

	storageNodeAddrs = flagutil.NewArrayString("storageNode", "Comma-separated list of TCP addresses for storage nodes to route the ingested logs to and to send select queries to. "+
		"If the list is empty, then the ingested logs are stored and queried locally from -storageDataPath")
	insertConcurrency        = flag.Int("insert.concurrency", 2, "The average number of concurrent data ingestion requests, which can be sent to every -storageNode")
	insertDisableCompression = flag.Bool("insert.disableCompression", false, "Whether to disable compression when sending the ingested data to -storageNode nodes. "+
		"Disabled compression reduces CPU usage at the cost of higher network usage")
	selectDisableCompression = flag.Bool("select.disableCompression", false, "Whether to disable compression for select query responses received from -storageNode nodes. "+
		"Disabled compression reduces CPU usage at the cost of higher network usage")

	storageNodeUsername     = flagutil.NewArrayString("storageNode.username", "Optional basic auth username to use for the corresponding -storageNode")
	storageNodePassword     = flagutil.NewArrayString("storageNode.password", "Optional basic auth password to use for the corresponding -storageNode")
	storageNodePasswordFile = flagutil.NewArrayString("storageNode.passwordFile", "Optional path to basic auth password to use for the corresponding -storageNode. "+
		"The file is re-read every second")
	storageNodeBearerToken     = flagutil.NewArrayString("storageNode.bearerToken", "Optional bearer auth token to use for the corresponding -storageNode")
	storageNodeBearerTokenFile = flagutil.NewArrayString("storageNode.bearerTokenFile", "Optional path to bearer token file to use for the corresponding -storageNode. "+
		"The token is re-read from the file every second")

	storageNodeTLS = flagutil.NewArrayBool("storageNode.tls", "Whether to use TLS (HTTPS) protocol for communicating with the corresponding -storageNode. "+
		"By default communication is performed via HTTP")
	storageNodeTLSCAFile = flagutil.NewArrayString("storageNode.tlsCAFile", "Optional path to TLS CA file to use for verifying connections to the corresponding -storageNode. "+
		"By default, system CA is used")
	storageNodeTLSCertFile = flagutil.NewArrayString("storageNode.tlsCertFile", "Optional path to client-side TLS certificate file to use when connecting "+
		"to the corresponding -storageNode")
	storageNodeTLSKeyFile    = flagutil.NewArrayString("storageNode.tlsKeyFile", "Optional path to client-side TLS certificate key to use when connecting to the corresponding -storageNode")
	storageNodeTLSServerName = flagutil.NewArrayString("storageNode.tlsServerName", "Optional TLS server name to use for connections to the corresponding -storageNode. "+
		"By default, the server name from -storageNode is used")
	storageNodeTLSInsecureSkipVerify = flagutil.NewArrayBool("storageNode.tlsInsecureSkipVerify", "Whether to skip tls verification when connecting to the corresponding -storageNode")
)

var localStorage *logstorage.Storage
var localStorageMetrics *metrics.Set

var netstorageInsert *netinsert.Storage

var netstorageSelect *netselect.Storage

// Init initializes vlstorage.
//
// Stop must be called when vlstorage is no longer needed
func Init() {
	if len(*storageNodeAddrs) == 0 {
		initLocalStorage()
	} else {
		initNetworkStorage()
	}
}

func initLocalStorage() {
	if localStorage != nil {
		logger.Panicf("BUG: initLocalStorage() has been already called")
	}

	if retentionPeriod.Duration() < 24*time.Hour {
		logger.Fatalf("-retentionPeriod cannot be smaller than a day; got %s", retentionPeriod)
	}
	// Validate mutually exclusive retention flags and their values
	if maxDiskSpaceUsageBytes.N > 0 && *maxDiskUsagePercent > 0 {
		logger.Fatalf("-retention.maxDiskSpaceUsageBytes and -retention.maxDiskUsagePercent cannot be set simultaneously")
	}
	if *maxDiskUsagePercent < 0 || *maxDiskUsagePercent > 100 {
		logger.Fatalf("-retention.maxDiskUsagePercent must be between 1 and 100; got %d", *maxDiskUsagePercent)
	}
	cfg := &logstorage.StorageConfig{
		Retention:              retentionPeriod.Duration(),
		MaxDiskSpaceUsageBytes: maxDiskSpaceUsageBytes.N,
		MaxDiskUsagePercent:    *maxDiskUsagePercent,
		FlushInterval:          *inmemoryDataFlushInterval,
		FutureRetention:        futureRetention.Duration(),
		LogNewStreams:          *logNewStreams,
		LogIngestedRows:        *logIngestedRows,
		MinFreeDiskSpaceBytes:  minFreeDiskSpaceBytes.N,
	}
	logger.Infof("opening storage at -storageDataPath=%s", *storageDataPath)
	startTime := time.Now()
	localStorage = logstorage.MustOpenStorage(*storageDataPath, cfg)

	var ss logstorage.StorageStats
	localStorage.UpdateStats(&ss)
	logger.Infof("successfully opened storage in %.3f seconds; smallParts: %d; bigParts: %d; smallPartBlocks: %d; bigPartBlocks: %d; smallPartRows: %d; bigPartRows: %d; "+
		"smallPartSize: %d bytes; bigPartSize: %d bytes",
		time.Since(startTime).Seconds(), ss.SmallParts, ss.BigParts, ss.SmallPartBlocks, ss.BigPartBlocks, ss.SmallPartRowsCount, ss.BigPartRowsCount,
		ss.CompressedSmallPartSize, ss.CompressedBigPartSize)

	// register local storage metrics
	localStorageMetrics = metrics.NewSet()
	localStorageMetrics.RegisterMetricsWriter(func(w io.Writer) {
		writeStorageMetrics(w, localStorage)
	})
	metrics.RegisterSet(localStorageMetrics)
}

func initNetworkStorage() {
	if netstorageInsert != nil || netstorageSelect != nil {
		logger.Panicf("BUG: initNetworkStorage() has been already called")
	}

	authCfgs := make([]*promauth.Config, len(*storageNodeAddrs))
	isTLSs := make([]bool, len(*storageNodeAddrs))
	for i := range authCfgs {
		authCfgs[i] = newAuthConfigForStorageNode(i)
		isTLSs[i] = storageNodeTLS.GetOptionalArg(i)
	}

	logger.Infof("starting insert service for nodes %s", *storageNodeAddrs)
	netstorageInsert = netinsert.NewStorage(*storageNodeAddrs, authCfgs, isTLSs, *insertConcurrency, *insertDisableCompression)

	logger.Infof("initializing select service for nodes %s", *storageNodeAddrs)
	netstorageSelect = netselect.NewStorage(*storageNodeAddrs, authCfgs, isTLSs, *selectDisableCompression)

	logger.Infof("initialized all the network services")
}

func newAuthConfigForStorageNode(argIdx int) *promauth.Config {
	username := storageNodeUsername.GetOptionalArg(argIdx)
	password := storageNodePassword.GetOptionalArg(argIdx)
	passwordFile := storageNodePasswordFile.GetOptionalArg(argIdx)
	var basicAuthCfg *promauth.BasicAuthConfig
	if username != "" || password != "" || passwordFile != "" {
		basicAuthCfg = &promauth.BasicAuthConfig{
			Username:     username,
			Password:     promauth.NewSecret(password),
			PasswordFile: passwordFile,
		}
	}

	token := storageNodeBearerToken.GetOptionalArg(argIdx)
	tokenFile := storageNodeBearerTokenFile.GetOptionalArg(argIdx)

	tlsCfg := &promauth.TLSConfig{
		CAFile:             storageNodeTLSCAFile.GetOptionalArg(argIdx),
		CertFile:           storageNodeTLSCertFile.GetOptionalArg(argIdx),
		KeyFile:            storageNodeTLSKeyFile.GetOptionalArg(argIdx),
		ServerName:         storageNodeTLSServerName.GetOptionalArg(argIdx),
		InsecureSkipVerify: storageNodeTLSInsecureSkipVerify.GetOptionalArg(argIdx),
	}

	opts := &promauth.Options{
		BasicAuth:       basicAuthCfg,
		BearerToken:     token,
		BearerTokenFile: tokenFile,
		TLSConfig:       tlsCfg,
	}
	ac, err := opts.NewConfig()
	if err != nil {
		logger.Panicf("FATAL: cannot populate auth config for storage node #%d: %s", argIdx, err)
	}

	return ac
}

// Stop stops vlstorage.
func Stop() {
	if localStorage != nil {
		metrics.UnregisterSet(localStorageMetrics, true)
		localStorageMetrics = nil

		localStorage.MustClose()
		localStorage = nil
	} else {
		netstorageInsert.MustStop()
		netstorageInsert = nil

		netstorageSelect.MustStop()
		netstorageSelect = nil
	}
}

// RequestHandler is a storage request handler.
func RequestHandler(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	switch path {
	case "/internal/force_merge":
		return processForceMerge(w, r)
	case "/internal/force_flush":
		return processForceFlush(w, r)
	case "/internal/partition/attach":
		return processPartitionAttach(w, r)
	case "/internal/partition/detach":
		return processPartitionDetach(w, r)
	case "/internal/partition/delete":
		return processPartitionDelete(w, r)
	case "/internal/partition/list":
		return processPartitionList(w, r)
	case "/internal/partition/snapshot/create":
		return processPartitionSnapshotCreate(w, r)
	case "/internal/partition/snapshot/list":
		return processPartitionSnapshotList(w, r)
	}
	return false
}

func processForceMerge(w http.ResponseWriter, r *http.Request) bool {
	if localStorage == nil {
		// Force merge isn't supported by non-local storage
		return false
	}

	if !httpserver.CheckAuthFlag(w, r, forceMergeAuthKey) {
		return true
	}

	// Run force merge in background
	partitionNamePrefix := r.FormValue("partition_prefix")
	go func() {
		activeForceMerges.Inc()
		defer activeForceMerges.Dec()
		logger.Infof("forced merge for partition_prefix=%q has been started", partitionNamePrefix)
		startTime := time.Now()
		localStorage.MustForceMerge(partitionNamePrefix)
		logger.Infof("forced merge for partition_prefix=%q has been successfully finished in %.3f seconds", partitionNamePrefix, time.Since(startTime).Seconds())
	}()
	return true
}

func processForceFlush(w http.ResponseWriter, r *http.Request) bool {
	if localStorage == nil {
		// Force merge isn't supported by non-local storage
		return false
	}

	if !httpserver.CheckAuthFlag(w, r, forceFlushAuthKey) {
		return true
	}

	logger.Infof("flushing storage to make pending data available for reading")
	localStorage.DebugFlush()
	return true
}

func processPartitionAttach(w http.ResponseWriter, r *http.Request) bool {
	if localStorage == nil {
		// There are no partitions in non-local storage
		return false
	}

	if !httpserver.CheckAuthFlag(w, r, partitionManageAuthKey) {
		return true
	}

	name := r.FormValue("name")
	if err := localStorage.PartitionAttach(name); err != nil {
		httpserver.Errorf(w, r, "%s", err)
		return true
	}

	return true
}

func processPartitionDelete(w http.ResponseWriter, r *http.Request) bool {
	if localStorage == nil {
		// There are no partitions in non-local storage
		return false
	}

	if !httpserver.CheckAuthFlag(w, r, partitionManageAuthKey) {
		return true
	}

	name := r.FormValue("name")
	if err := localStorage.PartitionDetach(name); err != nil {
		httpserver.Errorf(w, r, "%s", err)
		return true
	}

	return true
}

func processPartitionDetach(w http.ResponseWriter, r *http.Request) bool {
	if localStorage == nil {
		// There are no partitions in non-local storage
		return false
	}

	if !httpserver.CheckAuthFlag(w, r, partitionManageAuthKey) {
		return true
	}

	name := r.FormValue("name")
	if err := localStorage.PartitionDetach(name); err != nil {
		httpserver.Errorf(w, r, "%s", err)
		return true
	}

	return true
}

func processPartitionList(w http.ResponseWriter, r *http.Request) bool {
	if localStorage == nil {
		// There are no partitions in non-local storage
		return false
	}

	if !httpserver.CheckAuthFlag(w, r, partitionManageAuthKey) {
		return true
	}

	ptNames := localStorage.PartitionList()
	if ptNames == nil {
		// This is needed in order to return `[]` instead of `null` to the client.
		ptNames = []string{}
	}

	writeJSONResponse(w, ptNames)
	return true
}

func processPartitionSnapshotCreate(w http.ResponseWriter, r *http.Request) bool {
	if localStorage == nil {
		// There are no partitions in non-local storage
		return false
	}

	if !httpserver.CheckAuthFlag(w, r, partitionManageAuthKey) {
		return true
	}

	name := r.FormValue("name")
	snapshotPath, err := localStorage.PartitionSnapshotCreate(name)
	if err != nil {
		httpserver.Errorf(w, r, "%s", err)
		return true
	}

	writeJSONResponse(w, snapshotPath)
	return true
}

func processPartitionSnapshotList(w http.ResponseWriter, r *http.Request) bool {
	if localStorage == nil {
		// There are no partitions in non-local storage
		return false
	}

	if !httpserver.CheckAuthFlag(w, r, partitionManageAuthKey) {
		return true
	}

	snapshotPaths := localStorage.PartitionSnapshotList()
	if snapshotPaths == nil {
		// This is needed in order to return `[]` instead of `null` to the client.
		snapshotPaths = []string{}
	}

	writeJSONResponse(w, snapshotPaths)
	return true
}

func writeJSONResponse(w http.ResponseWriter, response any) {
	responseBody, err := json.Marshal(response)
	if err != nil {
		logger.Panicf("BUG: unexpected error when marshaling response %T: %s", response, err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(responseBody)
}

// Storage implements insertutil.LogRowsStorage interface
type Storage struct{}

// CanWriteData returns non-nil error if it cannot write data to vlstorage
func (*Storage) CanWriteData() error {
	if localStorage == nil {
		// The data can be always written in non-local mode.
		return nil
	}

	if localStorage.IsReadOnly() {
		return &httpserver.ErrorWithStatusCode{
			Err: fmt.Errorf("cannot add rows into storage in read-only mode; the storage can be in read-only mode "+
				"because of lack of free disk space at -storageDataPath=%s", *storageDataPath),
			StatusCode: http.StatusTooManyRequests,
		}
	}
	return nil
}

// MustAddRows adds lr to vlstorage
//
// It is advised to call CanWriteData() before calling MustAddRows()
func (*Storage) MustAddRows(lr *logstorage.LogRows) {
	if localStorage != nil {
		// Store lr in the local storage.
		localStorage.MustAddRows(lr)
	} else {
		// Store lr across the remote storage nodes.
		lr.ForEachRow(netstorageInsert.AddRow)
	}
}

// RunQuery runs the given qctx and calls writeBlock for the returned data blocks
func RunQuery(qctx *logstorage.QueryContext, writeBlock logstorage.WriteDataBlockFunc) error {
	qOpt, offset, limit := qctx.Query.GetLastNResultsQuery()
	if qOpt != nil {
		qctxOpt := qctx.WithQuery(qOpt)
		return runOptimizedLastNResultsQuery(qctxOpt, offset, limit, writeBlock)
	}

	if localStorage != nil {
		return localStorage.RunQuery(qctx, writeBlock)
	}
	return netstorageSelect.RunQuery(qctx, writeBlock)
}

// GetFieldNames executes qctx and returns field names seen in results.
func GetFieldNames(qctx *logstorage.QueryContext) ([]logstorage.ValueWithHits, error) {
	if localStorage != nil {
		return localStorage.GetFieldNames(qctx)
	}
	return netstorageSelect.GetFieldNames(qctx)
}

// GetFieldValues executes the given qctx and returns unique values for the fieldName seen in results.
//
// If limit > 0, then up to limit unique values are returned.
func GetFieldValues(qctx *logstorage.QueryContext, fieldName string, limit uint64) ([]logstorage.ValueWithHits, error) {
	if localStorage != nil {
		return localStorage.GetFieldValues(qctx, fieldName, limit)
	}
	return netstorageSelect.GetFieldValues(qctx, fieldName, limit)
}

// GetStreamFieldNames executes the given qctx and returns stream field names seen in results.
func GetStreamFieldNames(qctx *logstorage.QueryContext) ([]logstorage.ValueWithHits, error) {
	if localStorage != nil {
		return localStorage.GetStreamFieldNames(qctx)
	}
	return netstorageSelect.GetStreamFieldNames(qctx)
}

// GetStreamFieldValues executes the given qctx and returns stream field values for the given fieldName seen in results.
//
// If limit > 0, then up to limit unique stream field values are returned.
func GetStreamFieldValues(qctx *logstorage.QueryContext, fieldName string, limit uint64) ([]logstorage.ValueWithHits, error) {
	if localStorage != nil {
		return localStorage.GetStreamFieldValues(qctx, fieldName, limit)
	}
	return netstorageSelect.GetStreamFieldValues(qctx, fieldName, limit)
}

// GetStreams executes the given qctx and returns streams seen in query results.
//
// If limit > 0, then up to limit unique streams are returned.
func GetStreams(qctx *logstorage.QueryContext, limit uint64) ([]logstorage.ValueWithHits, error) {
	if localStorage != nil {
		return localStorage.GetStreams(qctx, limit)
	}
	return netstorageSelect.GetStreams(qctx, limit)
}

// GetStreamIDs executes the given qctx and returns streamIDs seen in query results.
//
// If limit > 0, then up to limit unique streamIDs are returned.
func GetStreamIDs(qctx *logstorage.QueryContext, limit uint64) ([]logstorage.ValueWithHits, error) {
	if localStorage != nil {
		return localStorage.GetStreamIDs(qctx, limit)
	}
	return netstorageSelect.GetStreamIDs(qctx, limit)
}

func writeStorageMetrics(w io.Writer, strg *logstorage.Storage) {
	var ss logstorage.StorageStats
	strg.UpdateStats(&ss)

	if ss.MaxDiskSpaceUsageBytes > 0 {
		metrics.WriteGaugeUint64(w, fmt.Sprintf(`vl_max_disk_space_usage_bytes{path=%q}`, *storageDataPath), uint64(ss.MaxDiskSpaceUsageBytes))
	}
	metrics.WriteGaugeUint64(w, fmt.Sprintf(`vl_free_disk_space_bytes{path=%q}`, *storageDataPath), fs.MustGetFreeSpace(*storageDataPath))
	metrics.WriteGaugeUint64(w, fmt.Sprintf(`vl_total_disk_space_bytes{path=%q}`, *storageDataPath), fs.MustGetTotalSpace(*storageDataPath))

	isReadOnly := uint64(0)
	if ss.IsReadOnly {
		isReadOnly = 1
	}
	metrics.WriteGaugeUint64(w, fmt.Sprintf(`vl_storage_is_read_only{path=%q}`, *storageDataPath), isReadOnly)

	metrics.WriteGaugeUint64(w, `vl_active_merges{type="storage/inmemory"}`, ss.ActiveInmemoryMerges)
	metrics.WriteGaugeUint64(w, `vl_active_merges{type="storage/small"}`, ss.ActiveSmallMerges)
	metrics.WriteGaugeUint64(w, `vl_active_merges{type="storage/big"}`, ss.ActiveBigMerges)
	metrics.WriteGaugeUint64(w, `vl_active_merges{type="indexdb/inmemory"}`, ss.IndexdbActiveInmemoryMerges)
	metrics.WriteGaugeUint64(w, `vl_active_merges{type="indexdb/file"}`, ss.IndexdbActiveFileMerges)

	metrics.WriteCounterUint64(w, `vl_merges_total{type="storage/inmemory"}`, ss.InmemoryMergesCount)
	metrics.WriteCounterUint64(w, `vl_merges_total{type="storage/small"}`, ss.SmallMergesCount)
	metrics.WriteCounterUint64(w, `vl_merges_total{type="storage/big"}`, ss.BigMergesCount)
	metrics.WriteCounterUint64(w, `vl_merges_total{type="indexdb/inmemory"}`, ss.IndexdbInmemoryMergesCount)
	metrics.WriteCounterUint64(w, `vl_merges_total{type="indexdb/file"}`, ss.IndexdbFileMergesCount)

	metrics.WriteCounterUint64(w, `vl_rows_merged_total{type="storage/inmemory"}`, ss.InmemoryRowsMerged)
	metrics.WriteCounterUint64(w, `vl_rows_merged_total{type="storage/small"}`, ss.SmallRowsMerged)
	metrics.WriteCounterUint64(w, `vl_rows_merged_total{type="storage/big"}`, ss.BigRowsMerged)
	metrics.WriteCounterUint64(w, `vl_rows_merged_total{type="indexdb/inmemory"}`, ss.IndexdbInmemoryItemsMerged)
	metrics.WriteCounterUint64(w, `vl_rows_merged_total{type="indexdb/file"}`, ss.IndexdbFileItemsMerged)

	metrics.WriteGaugeUint64(w, `vl_storage_rows{type="storage/inmemory"}`, ss.InmemoryRowsCount)
	metrics.WriteGaugeUint64(w, `vl_storage_rows{type="storage/small"}`, ss.SmallPartRowsCount)
	metrics.WriteGaugeUint64(w, `vl_storage_rows{type="storage/big"}`, ss.BigPartRowsCount)

	metrics.WriteGaugeUint64(w, `vl_storage_parts{type="storage/inmemory"}`, ss.InmemoryParts)
	metrics.WriteGaugeUint64(w, `vl_storage_parts{type="storage/small"}`, ss.SmallParts)
	metrics.WriteGaugeUint64(w, `vl_storage_parts{type="storage/big"}`, ss.BigParts)

	metrics.WriteGaugeUint64(w, `vl_storage_blocks{type="storage/inmemory"}`, ss.InmemoryBlocks)
	metrics.WriteGaugeUint64(w, `vl_storage_blocks{type="storage/small"}`, ss.SmallPartBlocks)
	metrics.WriteGaugeUint64(w, `vl_storage_blocks{type="storage/big"}`, ss.BigPartBlocks)

	metrics.WriteGaugeUint64(w, `vl_pending_rows{type="storage"}`, ss.PendingRows)
	metrics.WriteGaugeUint64(w, `vl_pending_rows{type="indexdb"}`, ss.IndexdbPendingItems)

	metrics.WriteGaugeUint64(w, `vl_partitions`, ss.PartitionsCount)
	metrics.WriteCounterUint64(w, `vl_streams_created_total`, ss.StreamsCreatedTotal)

	metrics.WriteGaugeUint64(w, `vl_indexdb_rows`, ss.IndexdbItemsCount)
	metrics.WriteGaugeUint64(w, `vl_indexdb_parts`, ss.IndexdbPartsCount)
	metrics.WriteGaugeUint64(w, `vl_indexdb_blocks`, ss.IndexdbBlocksCount)

	metrics.WriteGaugeUint64(w, `vl_data_size_bytes{type="indexdb"}`, ss.IndexdbSizeBytes)
	metrics.WriteGaugeUint64(w, `vl_data_size_bytes{type="storage"}`, ss.CompressedInmemorySize+ss.CompressedSmallPartSize+ss.CompressedBigPartSize)

	metrics.WriteGaugeUint64(w, `vl_compressed_data_size_bytes{type="storage/inmemory"}`, ss.CompressedInmemorySize)
	metrics.WriteGaugeUint64(w, `vl_compressed_data_size_bytes{type="storage/small"}`, ss.CompressedSmallPartSize)
	metrics.WriteGaugeUint64(w, `vl_compressed_data_size_bytes{type="storage/big"}`, ss.CompressedBigPartSize)

	metrics.WriteGaugeUint64(w, `vl_uncompressed_data_size_bytes{type="storage/inmemory"}`, ss.UncompressedInmemorySize)
	metrics.WriteGaugeUint64(w, `vl_uncompressed_data_size_bytes{type="storage/small"}`, ss.UncompressedSmallPartSize)
	metrics.WriteGaugeUint64(w, `vl_uncompressed_data_size_bytes{type="storage/big"}`, ss.UncompressedBigPartSize)

	metrics.WriteCounterUint64(w, `vl_rows_dropped_total{reason="too_big_timestamp"}`, ss.RowsDroppedTooBigTimestamp)
	metrics.WriteCounterUint64(w, `vl_rows_dropped_total{reason="too_small_timestamp"}`, ss.RowsDroppedTooSmallTimestamp)
}

var activeForceMerges = metrics.NewCounter("vl_active_force_merges")
