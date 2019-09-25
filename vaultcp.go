package main

// See: https://godoc.org/github.com/hashicorp/vault/api

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/hashicorp/vault/api"
	"github.com/gorilla/mux"
)

const maxUploadSize = 2 * 1024 * 1024 // 2 mb

var (
	working    bool = false
	kvRoot     string = ""
	kvApi      bool   = false
	srcClients []*api.Client
	dstClients []*api.Client
	listFile   *os.File
	versionString string

	// flags below
        kvRootFlag     *string
	listenPort     *int
	numWorkers     *int
	version        string
	doCopy         *bool
	doMirror       *bool
	srcInputFile   *string
	srcVaultAddr   *string
	dstVaultAddr   *string
	srcVaultToken  *string
	dstVaultToken  *string
	listOutputFile *string
)

func list2(path string) (err error) {
	srcKV := map[string]interface{}{}
	err = list(srcClients[0], path, false, srcKV)
	if err != nil {
		return err
	}

	jobMaps := make([]map[string]interface{}, *numWorkers)
	for i := 0; i < *numWorkers; i++ {
		jobMaps[i] = make(map[string]interface{}, 1000)
	}

	count := 0
	for k, sv := range srcKV {
		count++
		jobMaps[count%*numWorkers][k] = sv
	}

	var wg sync.WaitGroup

	for w := 0; w < *numWorkers; w++ {
		wg.Add(1)
		go listWorker(w, jobMaps[w], &wg)
	}

	wg.Wait()

	return err //assert nil
}

func copy(path string) (err error) {
	srcKV := map[string]interface{}{}

	if *srcInputFile != "" {
		err = listFromFile(srcKV)
		if err != nil {
			return err
		}
	} else {
		err = list(srcClients[0], path, false, srcKV)
		if err != nil {
			return err
		}
	}

	// TODO: determine if these are in the source Vault and if so optionally overwrite them if the data is different
	dstKV := map[string]interface{}{}
	err = list(dstClients[0], path, false, dstKV)
	if err != nil {
		return err
	}

	numsrckeys := len(srcKV)
	log.Printf("Info: The source Vault has %d keys\n", numsrckeys)

	numdstkeys := len(dstKV)
	log.Printf("Info: The destination Vault has %d keys\n", numdstkeys)

	// Divide the kv entries into numWorkers so we can update the dst Vault in parallel
	jobMaps := make([]map[string]interface{}, *numWorkers)
	for i := 0; i < *numWorkers; i++ {
		jobMaps[i] = make(map[string]interface{}, 1000)
	}

	count := 0
	for k, sv := range srcKV {
		var ok bool
		_, ok = dstKV[k]
		if ! ok {
			// k, v entry is missing from dst so register a job to copy it
			log.Printf("Copying key %s from source to dest Vault (it is missing from dest)\n", k)
			count++
			jobMaps[count%*numWorkers][k] = sv // sv will be non nil when read from input file
		}
	}

	var wg sync.WaitGroup

	for w := 0; w < *numWorkers; w++ {
		wg.Add(1)
		go writeWorker(w, jobMaps[w], &wg)
	}

	wg.Wait()

	return err //assert nil
}

func listWorker(id int, job map[string]interface{}, wg *sync.WaitGroup) {
	log.Println("list worker", id, "starting job of ", len(job), " keys")
	for k, v := range job {
		// Assert v == nil; lazy read to help parallelization
		log.Printf("list worker %d reading %s\n", id, k)
		var err error
		v, err = readRaw(srcClients[id], k)
		if err != nil {
			log.Printf("Error from readRaw: %s\n", err)
		}

		// print just the data element as we expect the metadata to be different, which would make determining diffs hard
		vComplete := v.(map[string]interface{})
		if kvApi {
			vData := vComplete["data"]
			v2, err := marshalData(vData.(map[string]interface{}))
			if err != nil {
				log.Printf("Error from marshalData: %s\n", err)
			}
			line := fmt.Sprintf("%s %s\n", k, v2)
			_, err = listFile.WriteString(line)
			if err != nil {
				log.Printf("Error from listFile.WriteString(%s): %s\n", line, err)
			}
		} else {
			v2, err := marshalData(vComplete)
			if err != nil {
				log.Printf("Error from marshalData: %s\n", err)
			}
			line := fmt.Sprintf("%s %s\n", k, v2)
			_, err = listFile.WriteString(line)
			if err != nil {
				log.Printf("Error from listFile.WriteString(%s): %s\n", line, err)
			}
		}
	}
	log.Println("list worker", id, "finished job of", len(job), " keys")
	wg.Done()
}

func writeWorker(id int, job map[string]interface{}, wg *sync.WaitGroup) {
	fmt.Println("write worker", id, "starting write job of ", len(job), " keys")
	var err error
	for k, v := range job {
		if v == nil || v == "" {
			log.Printf("write worker %d reading %s\n", id, k)
			v, err = readRaw(srcClients[id], k)
			if err != nil {
				log.Printf("Error from readRaw: %s\n", err)
			}
		}
		log.Printf("!!! write worker %d writing key %s\n", id, k)
		_, err = dstClients[id].Logical().Write(k, v.(map[string]interface{}))
		if err != nil {
			log.Printf("Error from Vault write: %s\n", err)
		}
	}
	fmt.Println("write worker", id, "finished write job of", len(job), " keys")
	wg.Done()
}

func listFromFile(kv map[string]interface{}) (err error) {

	f, err := os.Open(*srcInputFile)
	if err != nil {
		return err
	}
	defer f.Close()

	reader := bufio.NewReader(f)

	for {
		str, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		}
		parts := strings.Split(str, " ")
		k := strings.TrimSuffix(parts[0], "\n")
		v := strings.TrimSuffix(parts[1], "\n")
		// TODO consider which version of kv api (currently supporting only v2)
		if kvApi {
			v = fmt.Sprintf("{\"data\":%s}", v)
		} else {
		}
		var x map[string]interface{}
		json.Unmarshal([]byte(v), &x)
		kv[k] = x
	}
	return err // nil
}

func list(client *api.Client, path string, outputAndRead bool, kv map[string]interface{}) (err error) {
	path = strings.TrimSuffix(path, "/")

	s, err := client.Logical().List(path)
	if err != nil {
		return err
	}

	if s == nil {
		return // no entries
	}

	ikeys := s.Data["keys"].([]interface{})

	for _, ik := range ikeys {
		k := fmt.Sprint(ik)
		if strings.HasSuffix(k, "/") {
			k2 := strings.TrimSuffix(k, "/")
			p2 := fmt.Sprintf("%s/%s", path, k2)
			err = list(client, p2, outputAndRead, kv)
			if err != nil {
				return err
			}
		} else {
			path2 := path
			if kvApi {
				path2 = strings.Replace(path, "metadata", "data", 1)
			}
			p2 := fmt.Sprintf("%s/%s", path2, k)
			kv[p2] = nil // Intent is to lazy read
			if outputAndRead {
				value, err := readRaw(client, p2)
				if err != nil {
					return err
				}
				kv[p2] = value
				v, err := marshalData(value)
				if err != nil {
					return err
				}
				fmt.Printf("%s %v\n", p2, v)
			}
		}
	}
	return err
}

func marshalData(data map[string]interface{}) (value string, err error) {
	ba, err := json.Marshal(data)
	if err != nil {
		return value, err
	}
	value = string(ba)
	return value, err
}

func read(client *api.Client, path string) (value string, err error) {
	data, err := readRaw(client, path)
	if err != nil {
		return value, err
	}
	return marshalData(data)
}

func readRaw(client *api.Client, path string) (value map[string]interface{}, err error) {
	s, err := client.Logical().Read(path)
	if err != nil {
		return value, err
	}

	value = s.Data

	return value, err
}

func fetchVersionInfo(client *api.Client) (kvApiLocal bool, kvRoot string, err error) {
	sys := client.Sys()
	if *kvRootFlag != "" {
		kvRoot = *kvRootFlag
	} else {
		mounts, err := sys.ListMounts()
		if err != nil {
			return kvApiLocal, kvRoot, err
		}

		for k, v := range mounts {
			if v.Type == "kv" {
				kvRoot = k
				break
			}
		}
	}

	healthResponse, err := sys.Health()
	if err != nil {
		return kvApiLocal, kvRoot, err
	}
	parts := strings.Split(healthResponse.Version, " ") // example: 0.9.5
	parts = strings.Split(parts[0], ".")
	majorVer, err := strconv.Atoi(parts[0])
	minorVer, err := strconv.Atoi(parts[1])
	kvApiLocal = majorVer > 0 || minorVer >= 10

	return kvApiLocal, kvRoot, err
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	//msg := "{\"a\":\"b\"}"
	msg := "{\"endpoints\":{\"health\":\"GET\",\"list\":\"GET\",\"copyfromfile\":\"POST\",\"copyfromvault\":\"POST\"}"
	io.WriteString(w, msg)
}

func setWorking(flag bool) {
	working = flag
}

func resetForListVaultAction(srcaddr, srctoken string) {
	*doCopy = false
	*doMirror = false
	*srcInputFile = ""
	*srcVaultAddr = srcaddr
	*dstVaultAddr = ""
	*srcVaultToken = srctoken
	*dstVaultToken = ""
}

func resetForFileCopyAction(outfile, dstaddr, dsttoken string) {
	*doCopy = true
	*doMirror = false
	*srcInputFile = outfile 
	*srcVaultAddr = ""
	*srcVaultToken = ""
	*dstVaultAddr = "dstAddr"
	*dstVaultToken = dsttoken
}

type VaultConnect struct {
	SrcAddr string `json:"srcAddr"`
	DstAddr string `json:"dstAddr"`
}

/*
 * Example call:
 * curl -X GET -v -d '{"srcAddr":"http://127.0.0.1:8200"}' -H "X-Vault-Token:root" http://127.0.0.1:6200/list
 */
func listHandler(w http.ResponseWriter, r *http.Request) {
	if working {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "busy: try again later")
		return
	}
	setWorking(true)
	defer setWorking(false)

	srctoken := r.Header.Get("X-Vault-Token")

	decoder := json.NewDecoder(r.Body)

	var vc VaultConnect
	err := decoder.Decode(&vc)
	if err != nil {
		log.Printf("%s", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	srcaddr := vc.SrcAddr

	resetForListVaultAction(srcaddr, srctoken)

	err = prepConnections()
	if err != nil {
		log.Printf("%s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = prepForAction()
	if err != nil {
		log.Printf("%s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = doAction()
	if err != nil {
		log.Printf("%s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	file, err := os.Open(*listOutputFile)
	if err != nil {
		log.Printf("%s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	io.Copy(w, file)

	w.Header().Set("Content-Type", "text/plain")
}

// curl -F file=@"/path/filename.txt" http://localhost:6200/copyfromfile
// TODO: a work in progress
func copyFromFileHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("Hello from copyFromFileHandler\n")
	if working {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "busy: try again later")
		return
	}
	setWorking(true)
	defer setWorking(false)

	// dsttoken := r.Header.Get("X-Vault-Token")

/*
	decoder := json.NewDecoder(r.Body)

	var vc VaultConnect
	err := decoder.Decode(&vc)
	if err != nil {
		log.Printf("%s", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
*/

/*
	r.ParseForm()
	for k,v := range r.Form {
		log.Printf("k = %s ; v = %s\n", k, v)
	}
*/


	// validate file size
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		log.Printf("Error %v\n", err)
		w.Write([]byte("FILE_TOO_BIG"))
		w.WriteHeader(http.StatusBadRequest)
		return
	}




/*
OSXRMAURMBP15:vaultcp rmauri$ curl -X POST -F file=@./skdrivedev-vaultcp.out -F data='{"dstAddr":"http://127.0.0.1:8200"};type=application/json' http://127.0.0.1:6200/copyfromfile
2019/09/24 14:05:38 Hello from copyFromFileHandler
2019/09/24 14:05:38 r.MultipartForm = &{Value:map[data:[{"dstAddr":"http://127.0.0.1:8200"}]] File:map[file:[0xc000166000]]}
*/

	m := r.MultipartForm
	log.Printf("r.MultipartForm = %+v\n", m)

/*
		//get the *fileheaders
		files := m.File["myfiles"]
		for i, _ := range files {


	addr := r.PostFormValue("addr")
	if addr == "" {
		w.Write([]byte("INVALID_ADDR"))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	log.Printf("addr = %+v\n")
*/

	// fileType := r.PostFormValue("type")
	file, _, err := r.FormFile("file")
	if err != nil {
		log.Printf("Error %v\n", err)
		w.Write([]byte("INVALID_FILE"))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// log.Printf("file=%v header=%v\n", file, header)

	defer file.Close()

	tempFile, err := ioutil.TempFile("/tmp", "vaultcp.out")
	if err != nil {
		log.Printf("Error %v\n", err)
		w.Write([]byte("CANT_SAVE_OUTPUT_FILE"))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tempFile.Close()

	// read all of the contents of our uploaded file into a byte array
	fileBytes, err := ioutil.ReadAll(file)
	if err != nil {
		log.Printf("Error %v\n", err)
		w.Write([]byte("CANT_READ_FILE"))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// write this byte array to our temporary file
	tempFile.Write(fileBytes)
	log.Printf("wrote %d bytes to temp file %v\n", len(fileBytes), tempFile)


//	w.Write([]byte(header.Filename))

/*
		// check file type, detectcontenttype only needs the first 512 bytes
		filetype := http.DetectContentType(fileBytes)
		switch filetype {
		case "image/jpeg", "image/jpg":
		case "image/gif", "image/png":
		case "application/pdf":
			break
		default:
			renderError(w, "INVALID_FILE_TYPE", http.StatusBadRequest)
			return
		}
		fileName := randToken(12)
		fileEndings, err := mime.ExtensionsByType(fileType)
		if err != nil {
			renderError(w, "CANT_READ_FILE_TYPE", http.StatusInternalServerError)
			return
		}
		newPath := filepath.Join(uploadPath, fileName+fileEndings[0])
		fmt.Printf("FileType: %s, File: %s\n", fileType, newPath)

		// write file
		newFile, err := os.Create(newPath)
		if err != nil {
			renderError(w, "CANT_WRITE_FILE", http.StatusInternalServerError)
			return
		}
		defer newFile.Close() // idempotent, okay to call twice
		if _, err := newFile.Write(fileBytes); err != nil || newFile.Close() != nil {
			renderError(w, "CANT_WRITE_FILE", http.StatusInternalServerError)
			return
		}
		w.Write([]byte("SUCCESS"))
*/

/*
	resetForFileCopyAction(header.Filename, vc.DstAddr, dsttoken)

	err = prepConnections()
	if err != nil {
		log.Printf("%s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = prepForAction()
	if err != nil {
		log.Printf("%s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = doAction()
	if err != nil {
		log.Printf("%s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
*/

	w.WriteHeader(http.StatusOK)
}

func copyFromVaultHandler(w http.ResponseWriter, r *http.Request) {
	if working {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "busy: try again later")
		return
	}
	setWorking(true)
	defer setWorking(false)

	vars := mux.Vars(r)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	msg := fmt.Sprintf("{\"copyFromVault\":{\"srcaddr\":\"%s\",\"dstaddr\":\"%s\"}}",
		vars["srcaddr"], vars["dstaddr"])
	io.WriteString(w, msg)
}

func flags() (out string, err error) {
	kvRootFlag = flag.String("kvRootFlag", "", "Root of secret path to consider. Set to like \"secret/skydrivedev\" (or appropriate)  when using a non-admin token (can't discover from the real kv root mount point)")

	listenPort = flag.Int("listenPort", 0, "Http Listen port (when > 0 act as a server)")
	numWorkers = flag.Int("numWorkers", 10, "Number of workers to enable parallel execution")
	doCopy = flag.Bool("doCopy", false, "Copy the secrets from the source to destination Vault (default: false)")
	doMirror = flag.Bool("doMirror", false, "Like doCopy but destination Vault entries not in the source Vault will be deleted (default: false)")
	srcInputFile = flag.String("srcInputFile", "", "Source input file to read from instead of srcVaultAddr,srceVaultToken (use with doCopy, doMirror)")
	srcVaultAddr = flag.String("srcVaultAddr", "", "Source Vault address (required except when using srcInputFile)")
	srcVaultToken = flag.String("srcVaultToken", "", "Source Vault token (required except when using srcInputFile)")
	dstVaultAddr = flag.String("dstVaultAddr", "", "Destination Vault address (required for doCopy and doMirror)")
	dstVaultToken = flag.String("dstVaultToken", "", "Destination Vault token (required for doCopy and doMirror)")
	listOutputFile = flag.String("listOutputFile", "/tmp/vaultcp.out", "File to write listing (suitable for use by srcInputFile)")
	flag.StringVar(&version, "v", "false", "set to \"true\" to print current version and exit")

	flag.Parse()

	if version == "true" {
		out = versionString
		return out, err
	}

	if *numWorkers < 1 {
		err = fmt.Errorf("Error: Illegal value %d for numWorkers; it must be > 0", *numWorkers)
		return out, err
	}

	if *srcInputFile != "" && *srcVaultAddr != "" {
		err = fmt.Errorf("Error: srcInputFile and srcVaultAddr are both defined. Use on or the other")
		return out, err
	}

	if *listenPort < 0 {
		err = fmt.Errorf("Error: Illegal listenPort value. It must be > 0 to act as a server listening on this port")
		return out, err
	} else if *listenPort == 0 && *srcInputFile == "" && *srcVaultAddr == "" {
		err = fmt.Errorf("Error: srcInputFile or srcVaultAddr must be defined")
		return out, err
	}

	if *srcInputFile != "" && *doCopy == false && *doMirror == false {
		err = fmt.Errorf("Error: srcInputFile must be specified together with either doCopy or doMirror")
		return out, err
	}

	return out, err // err == nil
}

/*
 * Call this for a single list or copy action
 * It is important that only one action is outstanding at a time
 * This tool currently supports concurrent processing for a single action only.
 * Any web request must deny action requests if a prior request is being served
 *
 * depends on prepConmnections having been previously invoked
 * depends on doCopy or doMirror to be defined
 * depends on srcVaultAddr, srcVaultToken dstVaultAdr, dstVaultToken be defined
 */
func prepForAction() (err error) {
	var srcKvApi bool
	var srcKvRoot string
	var dstKvApi bool
	var dstKvRoot string

	if *doCopy || *doMirror {
		if *dstVaultAddr == "" {
			err = fmt.Errorf("Unspecified dstVaultAddr")
			return err
		}

		if *dstVaultToken == "" {
			err = fmt.Errorf("Unspecified dstVaultToken")
			return err
		}

		dstKvApi, dstKvRoot, err = fetchVersionInfo(dstClients[0])
		if err != nil {
			err = fmt.Errorf("Error fetching version info: %s", err)
			return err
		}

		if *srcVaultAddr != "" {
			srcKvApi, srcKvRoot, err = fetchVersionInfo(srcClients[0])
			if err != nil {
				err = fmt.Errorf("Error fetching version info: %s", err)
				return err
			}
			if dstKvApi != srcKvApi {
				err = fmt.Errorf("The Vault kv api is different betwen the source and destination Vaults")
				return err
			}
			if dstKvRoot != srcKvRoot {
				err = fmt.Errorf("The Vault kv root is different betwen the source and destination Vaults\n")
				return err
			}
			kvApi = srcKvApi
			kvRoot = srcKvRoot
		} else if *srcInputFile == "" {
			err = fmt.Errorf("You must specifiy either a srcInputFile or srcVaultAddr\n")
			return err
		} else {
			// we will read from srcInputFile
			kvApi = dstKvApi
			kvRoot = dstKvRoot
		}
	} else {
		// listing src vault mode
		if *srcVaultAddr != "" {
			srcKvApi, srcKvRoot, err = fetchVersionInfo(srcClients[0])
			if err != nil {
				err = fmt.Errorf("Error fetching version info: %s", err)
				return err
			}
			kvApi = srcKvApi
			kvRoot = srcKvRoot
		} // else case will not happen as per the earlier prepConnections chceck
	}
	return err // nil
}

func prepConnections() (err error) {
	var srcClient *api.Client
	var dstClient *api.Client

	srcClients = make([]*api.Client, *numWorkers)
	dstClients = make([]*api.Client, *numWorkers)
	for i := 0; i < *numWorkers; i++ {
		if *srcVaultAddr != "" {
			srcClient, err = api.NewClient(&api.Config{
				Address: *srcVaultAddr,
			})
			if err != nil {
				err = fmt.Errorf("Error from vault NewClient : %s\n", err)
				return err
			}
			srcClient.SetToken(*srcVaultToken)
			srcClients[i] = srcClient
		} // else we do not need srcClient connections as we will read from srcInputFile

		if *doCopy || *doMirror {
			dstClient, err = api.NewClient(&api.Config{
				Address: *dstVaultAddr,
			})
			if err != nil {
				err = fmt.Errorf("Error from vault NewClient : %s\n", err)
				return err
			}
			dstClient.SetToken(*dstVaultToken)
			dstClients[i] = dstClient
		}
	}

	return err // nil
}

/*
 * Depends on prepConnections and prepForAction having been previously invoked
 */
func doAction() (err error) {
	var path string
	if kvApi {
		if strings.HasSuffix(kvRoot, "/") {
			path = fmt.Sprintf("%smetadata", kvRoot)
		} else {
			path = fmt.Sprintf("%s/metadata", kvRoot)
		}
	} else {
		path = kvRoot
	}

	if *doCopy || *doMirror {
		err = copy(path)
		if err != nil {
			err = fmt.Errorf("Error copying secrets: %s", err)
			return err
		}
	} else {
		listFile, err = os.Create(*listOutputFile)
		if err != nil {
			err = fmt.Errorf("Error creating list output file %s: %s", *listOutputFile, err)
			return err
		}
		defer listFile.Close()

		err = list2(path)
		if err != nil {
			err = fmt.Errorf("Error listing secrets: %s", err)
			return err
		}
	}
	return err // nil
}

func runServer() {
	r := mux.NewRouter()
	r.HandleFunc("/health", healthHandler).Methods(http.MethodGet)
	r.HandleFunc("/list", listHandler).Methods(http.MethodGet)
	r.HandleFunc("/copyfromfile", copyFromFileHandler).Methods(http.MethodPost)
	r.HandleFunc("/copyfromvault/{srcaddr}/{dstaddr}", copyFromVaultHandler).Methods(http.MethodPost)
	addr  := fmt.Sprintf("localhost:%d", *listenPort)
	log.Fatal(http.ListenAndServe(addr, r))
}

func main() {
	var err error

	out, err := flags()
	if err != nil {
		log.Printf("%s", err)
		os.Exit(1)
	}
	if out != "" {
		log.Printf("%s", out)
		os.Exit(0)
	}

	if *listenPort > 0 {
		runServer()
	}

	// This tool is running in client mode so only a single action will be performed (a list or copy of one flavor or another)
	// This sequence will be the same for each web request (assuming no outstanding request is in progress)
	err = prepConnections()
	if err != nil {
		log.Printf("%s", err)
		os.Exit(1)
	}

	err = prepForAction()
	if err != nil {
		log.Printf("%s", err)
		os.Exit(1)
	}

	err = doAction()
	if err != nil {
		log.Printf("%s", err)
		os.Exit(1)
	}
}
