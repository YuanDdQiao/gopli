package command

import (
	"bufio"
	"bytes"
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/codegangsta/cli"
	"github.com/k0kubun/pp"
	"golang.org/x/crypto/ssh"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var fromDBConf Database
var fromSSHConf SSH
var toDBConf Database
var toSSHConf SSH

type tomlConfig struct {
	Database map[string]Database
	SSH      map[string]SSH
}

// Database settings
type Database struct {
	Host             string
	ManagementSystem string
	Name             string
	User             string
	Password         string
	Offset           int
}

// SSH settings
type SSH struct {
	Host string
	Port string
	User string
	Key  string
}

var listTableResultFile string
var loadDirName string
var fromHostConn *ssh.Client
var toHostConn *ssh.Client
var tableBlackList = [3]string{"schema_migrations", "repli_chk", "repli_clock"}

const (
	SelectTablesSQL      = "mysql -u%s -p%s -B -N -e 'SELECT * FROM %s.%s'"
	ShowTableSQL         = "mysql %s -u%s -p%s -B -N -e 'show tables'"
	MaxFetchSession      = 3
	MaxDeleteSession     = 3
	MaxLoadInfileSession = 3
	DefaultOffset        = 1000000000
	DeleteTableSQL       = "mysql -u%s -p%s -B -N -e 'DELETE FROM %s.%s'"
	LoadInfileQuery      = "LOAD DATA LOCAL INFILE '%s' INTO TABLE %s.%s"
	LoadInfileSession    = "mysql -u%s -p%s -h%s"
	ToHostMysqlConnect   = "%s:%s@tcp(%s:%s)/%s"
)

// CmdSync supports `sync` command in CLI
func CmdSync(c *cli.Context) {
	setupMultiCore()
	loadTomlConf(c)
	connectToFromHost()
	defer fromHostConn.Close()
	fetchTableList(fromHostConn)
	defer deleteTmpDir()
	fetchTables(fromHostConn)
	connectToToHost()
	defer toHostConn.Close()
	deleteTables(toHostConn)
	loadInfile(toHostConn)
}

func setupMultiCore() {
	maxProcs := os.Getenv("GOMAXPROCS")

	if maxProcs == "" {
		cpus := runtime.NumCPU()
		runtime.GOMAXPROCS(cpus)
	}
}

func readLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if isInBlackList(scanner.Text()) {
			continue
		}
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

func isInBlackList(table string) bool {
	for _, blackListElem := range tableBlackList {
		if blackListElem == table {
			return true
		}
	}
	return false
}

func loadTomlConf(c *cli.Context) {
	log.Print("[Setting] loading toml configuration...")
	var tmlconf tomlConfig
	if _, err := toml.DecodeFile(c.String("config"), &tmlconf); err != nil {
		pp.Print(err)
	}

	fromDBConf = tmlconf.Database[c.String("from")]
	toDBConf = tmlconf.Database[c.String("to")]
	fromSSHConf = tmlconf.SSH[c.String("from")]
	toSSHConf = tmlconf.SSH[c.String("to")]
	log.Print("[Setting] loaded toml configuration")
}

func loadFromSSHConf() *ssh.ClientConfig {
	usr, _ := user.Current()
	keypathString := strings.Replace(fromSSHConf.Key, "~", usr.HomeDir, 1)
	keypath, _ := filepath.Abs(keypathString)
	key, err := ioutil.ReadFile(keypath)
	if err != nil {
		log.Fatalf("unable to read private key: %v", err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		log.Fatalf("unable to parse private key: %v", err)
	}

	config := &ssh.ClientConfig{
		User: fromSSHConf.User,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
	}
	return config
}

func connectToFromHost() {
	config := loadFromSSHConf()
	conn, err := ssh.Dial("tcp", fromSSHConf.Host+":"+fromSSHConf.Port, config)
	if err != nil {
		panic("Failed to dial: " + err.Error())
	}
	fromHostConn = conn
}

func fetchTableList(conn *ssh.Client) {
	log.Print("[Fetch] fetching the list of tables...")
	session, err := conn.NewSession()
	if err != nil {
		panic("Failed to create session: " + err.Error())
	}
	defer session.Close()

	var listTableStdoutBuf bytes.Buffer
	session.Stdout = &listTableStdoutBuf
	listTableCmd := fmt.Sprintf(ShowTableSQL, fromDBConf.Name, fromDBConf.User, fromDBConf.Password)
	err = session.Run(listTableCmd)

	syncTimestamp := strconv.FormatInt(time.Now().Unix(), 10)
	loadDirName = "/tmp/db_sync_" + syncTimestamp
	if err := os.MkdirAll(loadDirName, 0777); err != nil {
		pp.Fatal(err)
	}

	listTableResultFile = loadDirName + "/" + fromDBConf.Name + "_list.txt"
	ioutil.WriteFile(listTableResultFile, listTableStdoutBuf.Bytes(), os.ModePerm)
	log.Print("[Fetch] completed fetching the list of tables")
}

func fetchTables(conn *ssh.Client) {
	log.Print("\t[Fetch] start to fetch table contents...")
	var tables []string
	tables, err := readLines(listTableResultFile)
	if err != nil {
		pp.Fatal(err)
	}

	sem := make(chan int, MaxFetchSession)
	var wg sync.WaitGroup
	for _, table := range tables {
		wg.Add(1)
		go func(table string) {
			sem <- 1
			defer wg.Done()
			defer func() { <-sem }()
			session, err := conn.NewSession()
			if err != nil {
				panic("Failed to create session: " + err.Error())
			}
			defer session.Close()

			var fetchTableStdoutBuf bytes.Buffer
			session.Stdout = &fetchTableStdoutBuf
			fetchRowsCmd := fmt.Sprintf(SelectTablesSQL, fromDBConf.User, fromDBConf.Password, fromDBConf.Name, table)
			log.Print("\t\t[Fetch] fetcing " + table)
			err = session.Run(fetchRowsCmd)
			if err != nil {
				pp.Fatal(err)
			}
			fetchTableRowsResultFile := loadDirName + "/" + fromDBConf.Name + "_" + table + ".txt"
			ioutil.WriteFile(fetchTableRowsResultFile, fetchTableStdoutBuf.Bytes(), os.ModePerm)
			log.Print("\t\t[Fetch] completed fetcing " + table)
		}(table)
	}
	wg.Wait()
	log.Print("\t[Fetch] completed fetching all tables")
}

func loadToSSHConf() *ssh.ClientConfig {
	usr, _ := user.Current()
	keypathString := strings.Replace(toSSHConf.Key, "~", usr.HomeDir, 1)
	keypath, _ := filepath.Abs(keypathString)
	key, err := ioutil.ReadFile(keypath)
	if err != nil {
		log.Fatalf("unable to read private key: %v", err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		log.Fatalf("unable to parse private key: %v", err)
	}

	config := &ssh.ClientConfig{
		User: toSSHConf.User,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
	}
	return config
}

func connectToToHost() {
	config := loadToSSHConf()
	conn, err := ssh.Dial("tcp", toSSHConf.Host+":"+toSSHConf.Port, config)
	if err != nil {
		panic("Failed to dial: " + err.Error())
	}
	toHostConn = conn
}

func deleteTables(conn *ssh.Client) {
	log.Print("[Delete] deleting existing tables...")
	var tables []string
	tables, err := readLines(listTableResultFile)
	if err != nil {
		pp.Fatal(err)
	}

	sem := make(chan int, 5)
	var wg sync.WaitGroup
	for _, table := range tables {
		wg.Add(1)
		go func(table string) {
			sem <- 1
			defer wg.Done()
			defer func() { <-sem }()
			session, err := conn.NewSession()
			if err != nil {
				panic("Failed to create session: " + err.Error())
			}
			defer session.Close()

			deleteTableCmd := fmt.Sprintf(DeleteTableSQL, toDBConf.User, toDBConf.Password, toDBConf.Name, table)
			var deleteTableStdoutBuf bytes.Buffer
			session.Stdout = &deleteTableStdoutBuf
			log.Print("\t[Delete] deleting " + table)
			err = session.Run(deleteTableCmd)
			if err != nil {
				pp.Fatal(err)
			}
		}(table)
	}
	wg.Wait()
	log.Print("[Delete] completed deleting tables")
}

func loadInfile(conn *ssh.Client) {
	log.Print("[Load Infile] start to send fetched contents...")
	var tables []string
	tables, err := readLines(listTableResultFile)
	if err != nil {
		pp.Fatal(err)
	}
	sem := make(chan int, MaxLoadInfileSession)
	var wg sync.WaitGroup
	for _, table := range tables {
		wg.Add(1)
		go func(table string) {
			sem <- 1
			defer wg.Done()
			defer func() { <-sem }()
			fetchedTableFile := loadDirName + "/" + fromDBConf.Name + "_" + table + ".txt"
			query := fmt.Sprintf(LoadInfileQuery, fetchedTableFile, toDBConf.Name, table)
			var passwordOption string
			if len(toDBConf.Password) > 0 {
				passwordOption = "-p"
			} else {
				passwordOption = ""
			}
			log.Print("\t[Load Infile] start to send the contents inside of " + table)
			cmd := exec.Command("mysql", "-uroot", passwordOption, "-h"+toSSHConf.Host, "--enable-local-infile", "--execute="+query)
			err := cmd.Run()
			if err != nil {
				pp.Fatal(err)
			}
			log.Print("\t[Load Infile] completed sending the contents inside of " + table)
		}(table)
		wg.Wait()
	}
	log.Print("[Load Infile] completed sending fetched contents")
	log.Print("[Finished] All tasks finished")
}

func isnil(x interface{}) bool {
	return x == nil || x == 0
}

func deleteTmpDir() {
	err := os.RemoveAll(loadDirName)
	if err != nil {
		pp.Print(err)
	}
}
