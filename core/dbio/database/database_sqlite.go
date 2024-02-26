package database

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/flarco/g/net"
	"github.com/samber/lo"
	"github.com/slingdata-io/sling-cli/core/dbio"
	"github.com/slingdata-io/sling-cli/core/dbio/filesys"
	"github.com/slingdata-io/sling-cli/core/dbio/iop"
	"github.com/spf13/cast"

	"github.com/flarco/g"

	"github.com/psanford/sqlite3vfs"
	"github.com/psanford/sqlite3vfshttp"
)

// SQLiteConn is a Google Big Query connection
type SQLiteConn struct {
	BaseConn
	URL string
}

const SQLiteVersion = "3.41.0"

// Init initiates the object
func (conn *SQLiteConn) Init() error {

	conn.BaseConn.URL = conn.URL
	conn.BaseConn.Type = dbio.TypeDbSQLite

	var instance Connection
	instance = conn
	conn.BaseConn.instance = &instance

	// handle S3 url
	if err := conn.setHttpURL(); err != nil {
		return g.Error(err, "could not set http url")
	}

	return conn.BaseConn.Init()
}

// GetURL returns the processed URL
func (conn *SQLiteConn) GetURL(newURL ...string) string {
	connURL := conn.BaseConn.URL
	if len(newURL) > 0 {
		connURL = newURL[0]
	}

	// fix scheme
	URL := strings.ReplaceAll(
		connURL,
		"sqlite://",
		"file:",
	)

	if httpURL := conn.GetProp("http_url"); httpURL != "" {
		URL = URL + "?vfs=httpvfs&mode=ro"
	}

	return URL
}

// BulkImportStream inserts a stream into a table
func (conn *SQLiteConn) BulkImportStream(tableFName string, ds *iop.Datastream) (count uint64, err error) {
	defer ds.Close()
	var columns iop.Columns

	bin, err := EnsureBinSQLite()
	if err != nil {
		g.DebugLow("sqlite3 not found in path. Using cursor... %s", err.Error())
		return conn.BaseConn.BulkImportStream(tableFName, ds)
	}

	conn.Commit()
	conn.Close()
	defer conn.Connect()

	table, err := ParseTableName(tableFName, conn.GetType())
	if err != nil {
		err = g.Error(err, "could not get table name for imoprt")
		return
	}

	// get file path
	dbPathU, err := net.NewURL(conn.BaseConn.URL)
	if err != nil {
		err = g.Error(err, "could not get sqlite file path")
		return
	}
	dbPath := strings.TrimPrefix(conn.GetURL(), "file:")
	dbPath = strings.ReplaceAll(dbPath, "?"+dbPathU.U.RawQuery, "")

	// need to clean up
	tempFiles := []string{}
	defer func() {
		for _, fp := range tempFiles {
			os.Remove(fp)
		}
	}()

	for batch := range ds.BatchChan {
		if batch.ColumnsChanged() || batch.IsFirst() {
			columns, err = conn.GetColumns(tableFName, batch.Columns.Names()...)
			if err != nil {
				return count, g.Error(err, "could not get list of columns from table")
			}

			err = batch.Shape(columns)
			if err != nil {
				return count, g.Error(err, "could not shape batch stream")
			}
		}

		cmd := exec.Command(bin)

		sameCols := g.Marshal(ds.Columns.Names(true, true)) == g.Marshal(columns.Names(true, true))

		// write to temp CSV
		csvPath := path.Join(getTempFolder(), g.NewTsID("sqlite.temp")+".csv")
		sqlPath := path.Join(getTempFolder(), g.NewTsID("sqlite.temp")+".sql")

		// set header. not needed if not creating a temp table
		cfgMap := ds.GetConfig()
		cfgMap["delimiter"] = ","
		cfgMap["bool_at_int"] = "true"
		cfgMap["header"] = lo.Ternary(sameCols, "false", "true")
		cfgMap["datetime_format"] = conn.GetProp("datetime_format")
		if strings.ToLower(cfgMap["datetime_format"]) == "auto" {
			cfgMap["datetime_format"] = "2006-01-02 15:04:05.000Z"
		}
		ds.SetConfig(cfgMap)

		if runtime.GOOS == "windows" {
			fs, err := filesys.NewFileSysClient(dbio.TypeFileLocal)
			if err != nil {
				err = g.Error(err, "could not obtain client for temp file")
				return 0, err
			}

			_, err = fs.Write("file://"+csvPath, ds.NewCsvReader(0, 0))
			if err != nil {
				err = g.Error(err, "could not write to temp file")
				return 0, err
			}
			tempFiles = append(tempFiles, csvPath)

		} else {
			csvPath = "/dev/stdin"
			cmd.Stdin = ds.NewCsvReader(0, 0)
		}

		tempTable := g.RandSuffix("temp_", 4)
		columnNames := lo.Map(columns.Names(), func(col string, i int) string {
			name, _ := ParseColumnName(col, conn.Type)
			return conn.Quote(name)
		})

		// set empty as null, since nulls are not ingested
		updateNulls := func(tName string) string {
			updateSQL := ""
			if cast.ToBool(ds.GetConfig()["empty_as_null"]) {
				updateCols := lo.Map(columnNames, func(c string, i int) string {
					return g.F(`%s = nullif(%s, '')`, c, c)
				})
				updateSQL = g.F(`update %s set %s where 1=1 ;`, tName, strings.Join(updateCols, ", "))
			}
			return updateSQL
		}

		sqlLines := []string{
			"PRAGMA journal_mode=WAL ;",
			g.F(".import --csv %s %s", csvPath, tempTable),
			updateNulls(tempTable),
			g.F(`insert into %s (%s) select * from %s ;`, table.Name, strings.Join(columnNames, ", "), tempTable),
			g.F("drop table %s ;", tempTable),
		}

		if sameCols {
			// no need for temp table
			sqlLines = []string{
				"PRAGMA journal_mode=WAL ;",
				g.F(".import --csv %s %s", csvPath, table.Name),
				updateNulls(table.Name),
			}
		}

		loadSQL := strings.Join(sqlLines, "\n")
		g.Trace(loadSQL)

		err = os.WriteFile(sqlPath, []byte(loadSQL), 0777)
		if err != nil {
			return 0, g.Error(err, "could not create load SQL for sqlite3")
		}
		tempFiles = append(tempFiles, sqlPath)

		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		cmd.Args = append(cmd.Args, dbPath, g.F(`.read %s`, sqlPath))
		out, err := cmd.Output()
		stderrVal := stderr.String()
		if err != nil {
			return 0, g.Error(err, "could not ingest csv file: %s\n%s", string(out), stderrVal)
		}
	}

	g.Trace("COPY %d ROWS", ds.Count)
	return ds.Count, nil
}

// GenerateUpsertSQL generates the upsert SQL
func (conn *SQLiteConn) GenerateUpsertSQL(srcTable string, tgtTable string, pkFields []string) (sql string, err error) {

	upsertMap, err := conn.BaseConn.GenerateUpsertExpressions(srcTable, tgtTable, pkFields)
	if err != nil {
		err = g.Error(err, "could not generate upsert variables")
		return
	}

	_, indexTable := SplitTableFullName(tgtTable)

	pkFieldsQ := lo.Map(pkFields, func(f string, i int) string { return conn.Quote(f) })
	indexSQL := g.R(
		conn.GetTemplateValue("core.create_unique_index"),
		"index", strings.Join(pkFields, "_")+g.RandSuffix("_", 3)+"_idx",
		"table", indexTable,
		"cols", strings.Join(pkFieldsQ, ", "),
	)

	_, err = conn.Exec(indexSQL)
	if err != nil {
		err = g.Error(err, "could not create unique index")
		return
	}

	sqlTempl := `
	INSERT INTO {tgt_table} as tgt
		({insert_fields}) 
	SELECT {src_fields}
	FROM {src_table} as src
	WHERE true
	ON CONFLICT ({pk_fields})
	DO UPDATE 
	SET {set_fields}
	`

	sql = g.R(
		sqlTempl,
		"src_table", srcTable,
		"tgt_table", tgtTable,
		"src_tgt_pk_equal", upsertMap["src_tgt_pk_equal"],
		"src_upd_pk_equal", strings.ReplaceAll(upsertMap["src_tgt_pk_equal"], "tgt.", "upd."),
		"src_fields", upsertMap["src_fields"],
		"pk_fields", upsertMap["pk_fields"],
		"set_fields", strings.ReplaceAll(upsertMap["set_fields"], "src.", "excluded."),
		"insert_fields", upsertMap["insert_fields"],
	)

	return
}

func writeTempSQL(sql string, filePrefix ...string) (sqlPath string, err error) {
	sqlPath = path.Join(getTempFolder(), g.NewTsID(filePrefix...)+".sql")

	err = os.WriteFile(sqlPath, []byte(sql), 0777)
	if err != nil {
		return "", g.Error(err, "could not create temp sql")
	}

	return
}

func (conn *SQLiteConn) setHttpURL() (err error) {

	httpURL := conn.GetProp("http_url")

	// handle S3 url
	if strings.HasPrefix(httpURL, "s3://") {

		expireDur := time.Minute
		if val := conn.GetProp("pre_signed_duration"); val != "" {
			mins := cast.ToInt64(val)
			expireDur = time.Duration(mins) * time.Minute
		}

		// need to generate pre-signed URL
		props := g.MapToKVArr(conn.Props())
		fs, err := filesys.NewFileSysClientFromURL(httpURL, props...)
		if err != nil {
			return g.Error(err, "could not connect to s3 bucket")
		}

		s3Fs, ok := fs.(*filesys.S3FileSysClient)
		if !ok {
			return g.Error("Could not convert to S3FileSysClient")
		}

		// check access
		paths, err := s3Fs.List(httpURL)
		if err != nil {
			return g.Error(err, "could not access s3 files")
		} else if len(paths) == 0 {
			return g.Error("Did not find any files with URL provided")
		}

		httpURL, err = s3Fs.GenerateS3PreSignedURL(httpURL, expireDur)
		if err != nil {
			return g.Error(err, "could not create Pre-Signed HTTP URL for s3 file")
		}
		conn.SetProp("http_url", httpURL)
	}

	if httpURL != "" {
		vfs := sqlite3vfshttp.HttpVFS{
			URL: httpURL,
			RoundTripper: &roundTripper{
				referer:   os.Getenv("DBIO_APP"),
				userAgent: os.Getenv("DBIO_APP"),
			},
		}

		err = sqlite3vfs.RegisterVFS("httpvfs", &vfs)
		if err != nil {
			return g.Error(err, "register vfs err")
		}
	}

	return nil
}

type roundTripper struct {
	referer   string
	userAgent string
}

func (rt *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.referer != "" {
		req.Header.Set("Referer", rt.referer)
	}

	if rt.userAgent != "" {
		req.Header.Set("User-Agent", rt.userAgent)
	}

	tr := http.DefaultTransport

	if req.URL.Scheme == "file" {
		path := req.URL.Path
		root := filepath.Dir(path)
		base := filepath.Base(path)
		tr = http.NewFileTransport(http.Dir(root))
		req.URL.Path = base
	}

	return tr.RoundTrip(req)
}

// EnsureBinSQLite ensures sqlite binary exists
// if missing, downloads and uses
func EnsureBinSQLite() (binPath string, err error) {
	folderPath := path.Join(g.UserHomeDir(), "sqlite")
	extension := lo.Ternary(runtime.GOOS == "windows", ".exe", "")
	binPath = path.Join(g.UserHomeDir(), "sqlite", "sqlite3"+extension)
	found := g.PathExists(binPath)

	defaultBin := func(name string) (string, error) {
		binPath, err := exec.LookPath(name)
		if err != nil {
			err = g.Error(err, "could not find sqlite3 bin")
		}
		return binPath, err
	}

	checkVersion := func() (bool, error) {

		out, err := exec.Command(binPath, "-version").Output()
		if err != nil {
			return false, g.Error(err, "could not get version for sqlite")
		}

		if strings.HasPrefix(string(out), SQLiteVersion) {
			return true, nil
		}

		return false, nil
	}

	// TODO: check version if found
	if found {
		ok, err := checkVersion()
		if err != nil {
			return "", g.Error(err, "error checking version for sqlite")
		}
		found = ok // so it can re-download if mismatch
	}

	if !found {
		// we need to download it ourselves
		var downloadURL string
		zipPath := path.Join(g.UserHomeDir(), "sqlite.zip")

		// all valid GOARCH -> https://gist.github.com/nictuku/c9858a4fe2c7b92a01da2e635b7c147c
		// compile steps: https://sqlite.org/forum/info/8b223b66319f05bf
		switch runtime.GOOS + "/" + runtime.GOARCH {

		case "windows/386":
			downloadURL = "https://www.sqlite.org/2023/sqlite-tools-win-x64-3440000.zip" // there is no preompiled x86?
		case "windows/amd64":
			downloadURL = "https://www.sqlite.org/2023/sqlite-tools-win-x64-3440000.zip"

		case "darwin/386":
			downloadURL = "https://www.sqlite.org/2023/sqlite-tools-osx-x86-3410000.zip"
		case "darwin/arm", "darwin/arm64":
			downloadURL = `https://ocral.nyc3.cdn.digitaloceanspaces.com/slingdata.io%2Fsqlite%2F3.41%2Fsqlite3-darwin-arm64-3.41.zip`

		case "linux/386":
			downloadURL = "https://www.sqlite.org/2023/sqlite-tools-linux-x86-3410000.zip"
		case "linux/amd64":
			downloadURL = `https://ocral.nyc3.cdn.digitaloceanspaces.com/slingdata.io%2Fsqlite%2F3.41%2Fsqlite-linux-amd64-3.41.zip`

		default:
			return defaultBin("sqlite3")
		}

		downloadURL = g.R(downloadURL, "version", SQLiteVersion)

		g.Info("downloading sqlite %s for %s/%s", SQLiteVersion, runtime.GOOS, runtime.GOARCH)
		err = net.DownloadFile(downloadURL, zipPath)
		if err != nil {
			return "", g.Error(err, "Unable to download sqlite binary")
		}

		paths, err := iop.Unzip(zipPath, folderPath)
		if err != nil {
			return "", g.Error(err, "Error unzipping sqlite zip")
		}

		for _, pathVal := range paths {
			if strings.HasSuffix(pathVal, "sqlite3") || strings.HasSuffix(pathVal, "sqlite3.exe") {
				err = os.Rename(pathVal, binPath)
				if err != nil {
					return "", g.Error(err, "Error renaming %s to %s", pathVal, binPath)
				}
				break
			}
		}

		if !g.PathExists(binPath) {
			return "", g.Error("cannot find %s, paths are: %s", binPath, g.Marshal(paths))
		}
	}

	_, err = checkVersion()
	if err != nil {
		return "", g.Error(err, "error checking version for sqlite")
	}

	return binPath, nil
}

// GetSchemata obtain full schemata info for a schema and/or table in current database
func (conn *SQLiteConn) GetSchemata(schemaName string, tableNames ...string) (Schemata, error) {
	schemata := Schemata{
		Databases: map[string]Database{},
		conn:      conn,
	}

	err := conn.Connect()
	if err != nil {
		return schemata, g.Error(err, "could not get connect to get schemata")
	}

	data, err := conn.GetSchemas()
	if err != nil {
		return schemata, g.Error(err, "could not get schemas")
	}

	schemaNames := data.ColValuesStr(0)
	if schemaName != "" {
		schemaNames = []string{schemaName}
	}

	schemas := map[string]Schema{}
	ctx := g.NewContext(conn.context.Ctx, 5)
	currDatabase := "main"

	getOneSchemata := func(values map[string]interface{}) {
		defer ctx.Wg.Read.Done()

		schemaData, err := conn.SumbitTemplate(
			"single", conn.template.Metadata, "schemata",
			values,
		)
		if err != nil {
			ctx.CaptureErr(g.Error(err, "Could not GetSchemata for "+schemaName))
		}

		defer ctx.Unlock()
		ctx.Lock()

		for _, rec := range schemaData.Records() {
			schemaName = cast.ToString(rec["schema_name"])
			tableName := cast.ToString(rec["table_name"])
			columnName := cast.ToString(rec["column_name"])
			dataType := strings.ToLower(cast.ToString(rec["data_type"]))

			switch v := rec["is_view"].(type) {
			case int64, float64:
				if cast.ToInt64(rec["is_view"]) == 0 {
					rec["is_view"] = false
				} else {
					rec["is_view"] = true
				}
			case string:
				if cast.ToBool(rec["is_view"]) {
					rec["is_view"] = true
				} else {
					rec["is_view"] = false
				}

			default:
				_ = fmt.Sprint(v)
				_ = rec["is_view"]
			}

			schema := Schema{
				Name:   schemaName,
				Tables: map[string]Table{},
			}

			table := Table{
				Name:     tableName,
				Schema:   schemaName,
				Database: currDatabase,
				IsView:   cast.ToBool(rec["is_view"]),
				Columns:  iop.Columns{},
				Dialect:  dbio.TypeDbSQLite,
			}

			if _, ok := schemas[strings.ToLower(schema.Name)]; ok {
				schema = schemas[strings.ToLower(schema.Name)]
			}

			if _, ok := schemas[strings.ToLower(schema.Name)].Tables[strings.ToLower(tableName)]; ok {
				table = schemas[strings.ToLower(schema.Name)].Tables[strings.ToLower(tableName)]
			}

			column := iop.Column{
				Name:     columnName,
				Type:     iop.ColumnType(conn.template.NativeTypeMap[dataType]),
				Table:    tableName,
				Schema:   schemaName,
				Database: currDatabase,
				Position: cast.ToInt(schemaData.Sp.ProcessVal(rec["position"])),
				DbType:   dataType,
			}

			table.Columns = append(table.Columns, column)

			schema.Tables[strings.ToLower(tableName)] = table
			schemas[strings.ToLower(schema.Name)] = schema
		}

		schemata.Databases[strings.ToLower(currDatabase)] = Database{
			Name:    currDatabase,
			Schemas: schemas,
		}
	}

	for _, schemaName := range schemaNames {
		g.Debug("getting schemata for %s", schemaName)
		values := g.M("schema", schemaName)

		if len(tableNames) > 0 && !(tableNames[0] == "" && len(tableNames) == 1) {
			tablesQ := []string{}
			for _, tableName := range tableNames {
				if strings.TrimSpace(tableName) == "" {
					continue
				}
				tablesQ = append(tablesQ, `'`+tableName+`'`)
			}
			if len(tablesQ) > 0 {
				values["tables"] = strings.Join(tablesQ, ", ")
			}
		}

		ctx.Wg.Read.Add()
		go getOneSchemata(values)
	}

	ctx.Wg.Read.Wait()

	if err := ctx.Err(); err != nil {
		return schemata, g.Error(err)
	}

	return schemata, nil
}

func getTempFolder() string {
	return cleanWindowsPath(strings.TrimRight(strings.TrimRight(os.TempDir(), "/"), "\\"))
}

func cleanWindowsPath(path string) string {
	return strings.ReplaceAll(path, `\`, `/`)
}
