// jpg2pdf_tool.go
//
// 完整的 Golang 实现：将指定案卷下的 JPG/PNG 文件合成 PDF，并处理清理、存储、并发、安全等逻辑。
// 要求：
// 1. 不跳过任何图片（包括小图）。
// 2. 不进行重试，如遇错误，直接记录并继续下一步。
// 3. 在 eam_record 表中，按照指定字段规则插入完整记录，不再包含 record_path 字段。
package main

import (
	"bufio"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/jpeg" // 注册 JPEG 解码器
	_ "image/png"  // 注册 PNG 解码器
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/signintech/gopdf"
	"github.com/tjfoc/gmsm/sm3"
	"gopkg.in/yaml.v2"
)

type Config struct {
	Database struct {
		DSN            string `yaml:"dsn"`
		TableInput     string `yaml:"table_input"`
		TableEAMFile   string `yaml:"table_eam_file"`
		TableEAMRecord string `yaml:"table_eam_record"`
		TableInputPDF  string `yaml:"table_input_pdf"`
	} `yaml:"database"`
	File struct {
		JPGRoot       string `yaml:"jpg_root"`
		PDFRoot       string `yaml:"pdf_root"`
		JPGBackupRoot string `yaml:"jpg_backup_root"`
		JPGAction     string `yaml:"jpg_action"` // move/delete/keep
	} `yaml:"file"`
	Folders struct {
		MaxPerDir int `yaml:"max_per_dir"`
	} `yaml:"folders"`
	Log struct {
		Path string `yaml:"path"`
	} `yaml:"log"`
	Concurrency int `yaml:"concurrency"`
}

// CaseInput 对应 file_input
type CaseInput struct {
	FileID       string
	FileAjdh     string
	ProcessType  int
	MoveJPG      int
	FileSliceImg string
}

// CaseDetail 对应 eam_file
type CaseDetail struct {
	FileID       string
	FileAjdh     string
	FileSProject string
	FileProject  string
	FileSliceImg string
}

type GlobalState struct {
	sliceMutex      sync.Mutex
	currentSliceDir string
	folderCount     int
}

var (
	cfg         Config
	db          *sql.DB
	globalState GlobalState
	totalCases  int
	processed   int
	progressMux sync.Mutex
	wg          sync.WaitGroup
	semaphore   chan struct{}
)

func main() {
	if err := loadConfig("config.yaml"); err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if err := initLogger(); err != nil {
		log.Fatalf("初始化日志失败: %v", err)
	}
	if err := initDB(); err != nil {
		log.Fatalf("数据库连接失败: %v", err)
	}
	defer db.Close()
	if err := initStorageState(); err != nil {
		log.Fatalf("初始化存储状态失败: %v", err)
	}
	cases, err := fetchPendingCases() //获取file_input表中待处理的案卷
	if err != nil {
		log.Fatalf("查询待处理案卷失败: %v", err)
	}
	totalCases = len(cases)
	log.Printf("共找到 %d 个待处理案卷\n", totalCases)
	go progressMonitor() // 启动进度打印协程
	semaphore = make(chan struct{}, cfg.Concurrency)
	for _, c := range cases {
		wg.Add(1)
		semaphore <- struct{}{}
		go func(caseIn CaseInput) {
			defer wg.Done()
			defer func() { <-semaphore }()
			processCase(caseIn)
		}(c)
	}
	wg.Wait()
	log.Println("所有案卷处理完成")
}

func loadConfig(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return yaml.NewDecoder(f).Decode(&cfg)
}

func initLogger() error {
	if cfg.Log.Path == "" {
		log.SetOutput(os.Stdout)
		return nil
	}
	f, err := os.OpenFile(cfg.Log.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	log.SetOutput(f)
	return nil
}

func initDB() error {
	var err error
	db, err = sql.Open("mysql", cfg.Database.DSN)
	if err != nil {
		return err
	}
	return db.Ping()
}

func fetchPendingCases() ([]CaseInput, error) {
	query := fmt.Sprintf(`
		SELECT file_id, file_ajdh, process_type, move_jpg, file_slice_image
		FROM %s
		WHERE outcome = 1;
	`, cfg.Database.TableInput)
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cases []CaseInput
	for rows.Next() {
		var c CaseInput
		if err := rows.Scan(&c.FileID, &c.FileAjdh, &c.ProcessType, &c.MoveJPG, &c.FileSliceImg); err != nil {
			return nil, err
		}
		cases = append(cases, c)
	}
	return cases, nil
}

// 从案卷表中获取案卷所属工程，所属项目，图片切片值，返回键值对，建是案卷ID，值是这些详细信息
func fetchCaseDetails(ids []string) (map[string]CaseDetail, error) {
	ph := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	// 这里是一次性获取，如果一次处理的数据量非常大，比如10万条，这里有可能会超出数据库上限或者非常慢
	query := fmt.Sprintf(`
		SELECT file_id, file_ajdh, file_sproject, file_project, file_slice_image
		FROM %s
		WHERE file_id IN (%s);
	`, cfg.Database.TableEAMFile, strings.Join(ph, ","))
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[string]CaseDetail)
	for rows.Next() {
		var d CaseDetail
		if err := rows.Scan(&d.FileID, &d.FileAjdh, &d.FileSProject, &d.FileProject, &d.FileSliceImg); err != nil {
			return nil, err
		}
		m[d.FileID] = d
	}
	return m, nil
}

func processCase(c CaseInput) {
	defer func() {
		if r := recover(); r != nil { // recorver的作用是收集代码中产生的panic，如果没有就是nil，如果有就返回内容
			log.Printf("案卷 %s 处理失败: %v", c.FileID, r)
			markCaseFailed(c.FileID, fmt.Sprintf("处理异常: %v", r))
		}
		progressMux.Lock()
		processed++
		progressMux.Unlock()
	}()

	detailsMap, err := fetchCaseDetails([]string{c.FileID})
	if err != nil {
		panic(fmt.Sprintf("根据输入案卷表查询案卷详细信息出错: %v", err))
	}
	detail := detailsMap[c.FileID]
	jpgPath := filepath.Join(cfg.File.JPGRoot, detail.FileSliceImg, c.FileID)
	if err := cleanupPDFRecords(c.FileID); err != nil { // 根据input_pdf表中的数据删除PDF文件及其所在文件夹，删除对应的数据库记录，如果有错误就跑出错误，此案卷处理失败
		panic(fmt.Sprintf("清理 PDF 失败: %v", err))
	}
	if c.ProcessType == 2 {
		inner := filepath.Join(jpgPath, "内部")
		// ???核实内部是否为1，外部是否为2
		if dirExists(inner) {
			err, len := mergeAndStorePDF(c.FileID, detail, jpgPath, 1)
			if err != nil {
				if len == 0 {
					log.Printf("合成外部 PDF 失败: %v", err)
				} else {
					panic(fmt.Sprintf("合成外部 PDF 失败: %v", err))
				}
				//log.Printf("合成外部 PDF 失败: %v", err)
			}
			if err, _ := mergeAndStorePDF(c.FileID, detail, inner, 2); err != nil {
				panic(fmt.Sprintf("合成内部 PDF 失败: %v", err))
				//log.Printf("合成内部 PDF 失败: %v", err)
			}
		} else {
			// ????
			if err, _ := mergeAndStorePDF(c.FileID, detail, jpgPath, 1); err != nil {
				panic(fmt.Sprintf("合成外部 PDF 失败: %v", err))
				//log.Printf("合成外部 PDF 失败: %v", err)
			}
		}
	}
	if c.MoveJPG == 1 {
		if err := handleJPGs(c.FileID, jpgPath); err != nil {
			// 这里是否应该panic
			panic(fmt.Sprint("JPG 处理失败: %v", err))
			//log.Printf("JPG 处理失败: %v", err)
		}
		if err := markCaseSuccess(c.FileID, true); err != nil {
			panic(fmt.Sprintf("严重错误: JPG 已处理但状态更新失败: %v", err))
		}
	} else {
		if err := markCaseSuccess(c.FileID, false); err != nil {
			panic(fmt.Sprint("更新 file_input 状态失败: %v", err))
		}
	}
}

func cleanupPDFRecords(caseID string) error {
	query := fmt.Sprintf(`
		SELECT record_id, record_path
		FROM %s
		WHERE record_file = ?;
	`, cfg.Database.TableInputPDF)
	rows, err := db.Query(query, caseID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var ids []string
	var paths []string
	for rows.Next() {
		var rid, rpath string
		if err := rows.Scan(&rid, &rpath); err != nil {
			return err
		}
		ids = append(ids, rid)
		paths = append(paths, rpath)
	}
	if len(ids) == 0 {
		fmt.Println("没有发现待清理的PDF", err)
		return nil
	}

	for _, p := range paths {
		if err := os.RemoveAll(p); err != nil && !os.IsNotExist(err) {
			log.Printf("删除 PDF 文件夹失败 (%s): %v", p, err)
			return err
		}
	}

	ph := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	del := fmt.Sprintf(`
		DELETE FROM %s
		WHERE record_id IN (%s);
	`, cfg.Database.TableEAMRecord, strings.Join(ph, ","))
	_, err = db.Exec(del, args...)
	return err
}

func mergeAndStorePDF(
	caseID string,
	detail CaseDetail,
	folderPath string,
	isInside int,
) (error, int) {
	files, err := os.ReadDir(folderPath)
	// 如果没有读取到JPG图片，就立即返回错误信息
	if err != nil {
		return err, 0
	}
	var imgs []string
	// 读取每张JPG图片，跳过目录
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f.Name()))
		if ext == ".jpg" || ext == ".jpeg" {
			imgs = append(imgs, f.Name())
		}
	}
	if len(imgs) == 0 {
		log.Printf("案卷 %s 文件夹 %s 无 JPG/PNG，跳过", caseID, folderPath)
		return fmt.Errorf("案卷 %s 文件夹 %s 无 JPG/PNG，跳过", caseID, folderPath), 0
	}
	sort.Strings(imgs)
	// 生成record_id
	recordID := generateUUID()
	// 根据record_id分配存储路径
	storagePath, sliceID, err := allocateStoragePath(recordID)
	if err != nil {
		return err, len(imgs)
	}
	// 创建上面生成的存存储路径
	if err := os.MkdirAll(storagePath, os.ModePerm); err != nil {
		return err, len(imgs)
	}
	pdf := gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})
	var badJPGs []string
	for _, name := range imgs {
		imgPath := filepath.Join(folderPath, name)
		f, err := os.Open(imgPath)
		if err != nil {
			log.Printf("打开图片失败: %s, %v", imgPath, err)
			return err, len(imgs)
		}
		cfgImg, _, err := image.DecodeConfig(bufio.NewReader(f))
		f.Close()

		if err != nil {
			// ??? 这个位置不能跳过打开失败的图片，如果遇到打开失败的图片是否继续需要核实？
			log.Printf("解析图片尺寸失败: %s, %v", imgPath, err)
			//return err, len(imgs)
			badJPGs = append(badJPGs, name)
		} else {
			w := float64(cfgImg.Width)
			h := float64(cfgImg.Height)
			pdf.AddPageWithOption(gopdf.PageOption{PageSize: &gopdf.Rect{W: w, H: h}})
			if err := pdf.Image(imgPath, 0, 0, &gopdf.Rect{W: w, H: h}); err != nil {
				log.Printf("写入 PDF 失败: %s, %v", imgPath, err)
				return fmt.Errorf("写入 PDF 失败: %s, %v", imgPath, err), len(imgs)
			}
		}
	}
	pdfFile := filepath.Join(storagePath, recordID+".pdf")
	if err := pdf.WritePdf(pdfFile); err != nil {
		return err, len(imgs)
	}
	pdfBytes, err := os.ReadFile(pdfFile)
	if err != nil {
		return err, len(imgs)
	}
	sum := md5.Sum(pdfBytes)
	md5Str := hex.EncodeToString(sum[:])
	sm3Hasher := sm3.New()
	sm3Hasher.Write(pdfBytes)
	sm3Sum := sm3Hasher.Sum(nil) // []byte 长度 32
	sm3Str := hex.EncodeToString(sm3Sum)
	pageCount := len(imgs)
	fileInfo, err := os.Stat(pdfFile)
	if err != nil {
		return err, len(imgs)
	}
	pdfSize := int(fileInfo.Size())
	err = insertEAMRecord(
		recordID, detail, sliceID, caseID,
		isInside, md5Str, sm3Str, pageCount, pdfSize, badJPGs,
	)
	return err, len(imgs)
}

func allocateStoragePath(recordID string) (string, string, error) {
	base := cfg.File.PDFRoot
	globalState.sliceMutex.Lock()
	defer globalState.sliceMutex.Unlock()
	if globalState.folderCount >= cfg.Folders.MaxPerDir {
		globalState.currentSliceDir = incrementDir(globalState.currentSliceDir)
		globalState.folderCount = 0
	}
	sliceID := globalState.currentSliceDir
	p := filepath.Join(base, sliceID, "source", recordID)
	globalState.folderCount++
	return p, sliceID, nil
}

func insertEAMRecord(
	recordID string,
	detail CaseDetail,
	storagePath, caseID string,
	isInside int,
	md5Str, sm3Str string,
	pageCount, pdfSize int,
	illustration []string,
) error {
	var maxSeq sql.NullInt64
	querySeq := fmt.Sprintf(`
		SELECT MAX(record_wj_xh)
		FROM %s
		WHERE record_file = ?;
	`, cfg.Database.TableEAMRecord)
	if err := db.QueryRow(querySeq, caseID).Scan(&maxSeq); err != nil && err != sql.ErrNoRows {
		return err
	}
	nextSeq := 1
	if maxSeq.Valid {
		nextSeq = int(maxSeq.Int64) + 1
	}
	seqStr := fmt.Sprintf("%03d", nextSeq)
	wjdh := fmt.Sprintf("%s-%s", detail.FileAjdh, seqStr)
	wjtm := "案卷合成文件（外部）"
	if isInside == 2 {
		wjtm = "案卷合成文件（内部）"
	}
	fz := fmt.Sprintf("PDF文件合成时间：%s,图片路径：%s", time.Now().Format("2006-01-02 15:04:05"), storagePath)
	if len(illustration) != 0 {
		fz = fmt.Sprintf("PDF文件合成时间：%s,图片路径：%s,存在损坏图片：%s", time.Now().Format("2006-01-02 15:04:05"), storagePath, strings.Join(illustration, ";"))
	}
	query := fmt.Sprintf(`
		INSERT INTO %s (
			record_id,
			archives_id,
			record_borrow_state,
			record_inventory_status,
			record_pdf_size,
			record_status,
			record_suffix,
			record_sys_from,
			record_wj_xh,
			record_wjdh,
			record_wjtm,
			record_fz,
			record_pdf_page,
			record_sl,
			record_slice_id,
			record_file,
			record_sproject,
			record_project,
			url,
			upload_server_id,
			record_server_type,
			record_real_path,
			sync_state,
			record_md5,
			record_sm3,
			record_is_full_text,
			record_isinside,
			record_is_filepdf,
			record_is_public,
			record_hcstatus,
			record_checkstatus,
			record_filestatus,
			record_updatemd5,
			record_sync,
			record_process,
			record_sfysj,
			record_is_mj
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
	`, cfg.Database.TableEAMRecord)

	_, err := db.Exec(query,
		recordID,            // record_id
		"001",               // archives_id
		0,                   // record_borrow_state
		35,                  // record_inventory_status
		pdfSize,             // record_pdf_size
		6,                   // record_status
		".pdf",              // record_suffix
		5,                   // record_sys_from
		nextSeq,             // record_wj_xh
		wjdh,                // record_wjdh
		wjtm,                // record_wjtm
		fz,                  // record_fz
		pageCount,           // record_pdf_page
		pageCount,           // record_sl
		storagePath,         // record_slice_id
		caseID,              // record_file
		detail.FileSProject, // record_sproject
		detail.FileProject,  // record_project
		"images",            // url
		4,                   // upload_server_id
		1,                   // record_server_type
		"J:\\imagess",       // record_real_path
		0,                   // sync_state
		md5Str,              // record_md5
		sm3Str,              // record_sm3 ✅ 新增
		0,                   // record_is_full_text
		isInside,            // record_isinside
		2,                   // record_is_filepdf
		1,                   // record_is_public
		1,                   // record_hcstatus
		2,                   // record_checkstatus
		2,                   // record_filestatus
		3,                   // record_updatemd5
		nil,                 // record_sync
		0,                   // record_process
		2,                   // record_sfysj
		1,                   // record_is_mj
	)
	return err
}

func handleJPGs(caseID, jpgPath string) error {
	action := strings.ToLower(cfg.File.JPGAction)
	switch action {
	case "move":
		relPath, err := filepath.Rel(cfg.File.JPGRoot, jpgPath)
		if err != nil {
			return fmt.Errorf("计算相对路径失败: %v", err)
		}
		//fmt.Println(cfg.File.JPGBackupRoot, relPath)
		dest := filepath.Join(cfg.File.JPGBackupRoot, relPath)
		if err := os.MkdirAll(filepath.Dir(dest), os.ModePerm); err != nil {
			return fmt.Errorf("创建目录失败: %v", err)
		}
		return os.Rename(jpgPath, dest)

	case "delete":
		return os.RemoveAll(jpgPath)

	case "keep":
		return nil

	default:
		return fmt.Errorf("未知的 JPG 操作类型: %s", cfg.File.JPGAction)
	}
}

func markCaseSuccess(caseID string, removeJPG bool) error {
	var query string
	var qeuryFile string
	var err error
	if removeJPG {
		query = fmt.Sprintf(`
			UPDATE %s
			SET outcome = 2,
			    process_time = NOW()
			WHERE file_id = ?;
		`, cfg.Database.TableInput)
		qeuryFile = fmt.Sprintf(`
			UPDATE %s
			SET file_slice_image = NULL
			WHERE file_id = ?;
		`, cfg.Database.TableEAMFile)
		_, err = db.Exec(query, caseID)
		_, err = db.Exec(qeuryFile, caseID)
	} else {
		query = fmt.Sprintf(`
			UPDATE %s
			SET outcome = 2,
			    process_time = NOW()
			WHERE file_id = ?;
		`, cfg.Database.TableInput)
		_, err = db.Exec(query, caseID)
	}

	return err
}

func markCaseFailed(caseID, reason string) {
	query := fmt.Sprintf(`
		UPDATE %s
		SET outcome = 3,
		    process_time = NOW(),
		    fail_reason = ?
		WHERE file_id = ?;
	`, cfg.Database.TableInput)
	if _, err := db.Exec(query, reason, caseID); err != nil {
		log.Printf("更新失败状态失败: %v", err)
	}
}

func progressMonitor() {
	ticker := time.NewTicker(5 * time.Second)
	for range ticker.C {
		progressMux.Lock()
		percent := 0.0
		if totalCases > 0 {
			percent = float64(processed) / float64(totalCases) * 100
		}
		fmt.Printf("进度: %d/%d (%.1f%%) 当前目录: %s (%d/%d)\n",
			processed, totalCases, percent,
			globalState.currentSliceDir, globalState.folderCount, cfg.Folders.MaxPerDir)
		progressMux.Unlock()
		if processed >= totalCases {
			ticker.Stop()
			return
		}
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

func incrementDir(dir string) string {
	num, err := strconv.Atoi(dir)
	if err != nil {
		log.Printf("解析目录号失败 (%s): %v", dir, err)
		return dir
	}
	num++
	return fmt.Sprintf("%05d", num)
}

func generateUUID() string {
	return uuid.New().String()
}

func initStorageState() error {
	base := filepath.Join(cfg.File.PDFRoot)
	entries, err := os.ReadDir(base)
	if err != nil {
		return fmt.Errorf("读取 PDF 根目录失败: %v", err)
	}

	var sliceDirs []int
	for _, entry := range entries {
		if entry.IsDir() {
			if num, err := strconv.Atoi(entry.Name()); err == nil {
				sliceDirs = append(sliceDirs, num)
			}
		}
	}

	if len(sliceDirs) == 0 {
		globalState.currentSliceDir = "00000"
		globalState.folderCount = 0
		return nil
	}

	sort.Ints(sliceDirs)
	maxDir := sliceDirs[len(sliceDirs)-1]
	globalState.currentSliceDir = fmt.Sprintf("%05d", maxDir)

	sourceDir := filepath.Join(base, globalState.currentSliceDir, "source")
	sourceEntries, err := os.ReadDir(sourceDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("读取 source 目录失败: %v", err)
	}

	count := 0
	for _, entry := range sourceEntries {
		if entry.IsDir() {
			count++
		}
	}
	globalState.folderCount = count
	if count >= cfg.Folders.MaxPerDir {
		// 当前目录已满，创建新目录
		globalState.currentSliceDir = fmt.Sprintf("%05d", maxDir+1)
		globalState.folderCount = 0
		log.Printf("目录 %05d 已满 (%d >= %d)，创建新目录: %s",
			maxDir, count, cfg.Folders.MaxPerDir, globalState.currentSliceDir)
	} else {
		// 当前目录未满，继续使用
		globalState.currentSliceDir = fmt.Sprintf("%05d", maxDir)
		globalState.folderCount = count
		log.Printf("使用现有目录: %s，已有文件夹数量: %d/%d",
			globalState.currentSliceDir, count, cfg.Folders.MaxPerDir)
	}
	log.Printf("初始化切片目录为 %s，已有文件夹数量 %d", globalState.currentSliceDir, globalState.folderCount)
	return nil
}

// func initStorageState() error {
// 	base := filepath.Join(cfg.File.PDFRoot)
// 	entries, err := os.ReadDir(base)
// 	if err != nil {
// 		return err
// 	}

// 	var maxDir string
// 	var maxCount int

// 	for _, entry := range entries {
// 		if !entry.IsDir() {
// 			continue
// 		}
// 		name := entry.Name()
// 		if _, err := strconv.Atoi(name); err != nil {
// 			continue
// 		}

// 		subDir := filepath.Join(base, name, "source")
// 		count := 0
// 		if files, err := os.ReadDir(subDir); err == nil {
// 			count = len(files)
// 		}

// 		if name > maxDir || maxDir == "" {
// 			maxDir = name
// 			maxCount = count
// 		}
// 	}

// 	if maxDir == "" {
// 		maxDir = "00001"
// 		maxCount = 0
// 	}

// 	globalState.currentSliceDir = maxDir
// 	globalState.folderCount = maxCount

// 	log.Printf("当前切片目录初始化为 %s，已使用 %d 个文件夹\n", maxDir, maxCount)
// 	return nil
// }
