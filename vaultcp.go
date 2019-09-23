package main

// See: https://godoc.org/github.com/hashicorp/vault/api

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/hashicorp/vault/api"
	"github.com/gorilla/mux"
)

var (
	working    bool = false
	kvRoot     string = ""
	kvApi      bool   = false
	srcClients []*api.Client
	dstClients []*api.Client
	listFile   *os.File
	versionString string

	// flags below
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

	numdstkeys := len(dstKV)
	if numdstkeys > 0 {
		log.Printf("Warning: The destination Vault already has %d keys\n", numdstkeys)
	}

	numsrckeys := len(srcKV)
	log.Printf("Info: The source Vault has %d keys\n", numsrckeys)

	if numsrckeys == 0 {
		log.Printf("Warning: The source Vault has no keys\n")
		// TODO delete entries in dest when doMirror
	}

	// Divide the kv entries into numWorkers so we can update the dst Vault in parallel
	jobMaps := make([]map[string]interface{}, *numWorkers)
	for i := 0; i < *numWorkers; i++ {
		jobMaps[i] = make(map[string]interface{}, 1000)
	}

	count := 0
	for k, sv := range srcKV {
		// TODO: don't list and read dest unless doMirror
		dv := dstKV[k]
		if dv == "" || dv == nil {
			log.Printf("Copy (for create) src to dst vault: %s value: %s\n", k, sv)
			count++
			jobMaps[count%*numWorkers][k] = sv
		} else if sv != dv {
			log.Printf("Copy (for update?) src to dst vault: %s value: %s\n", k, sv)
			count++
			jobMaps[count%*numWorkers][k] = sv
		} else {
			log.Printf("The src and dst Vaults have a matching kv entry for key %s\n", k)
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
		} else {
		}
		log.Printf("!!! write worker %d writing %s => %+v\n", id, k, v)
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

func listHandler(w http.ResponseWriter, r *http.Request) {
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
	msg := fmt.Sprintf("{\"list\":{\"srcaddr\":\"%s\"}}", vars["srcaddr"])
	io.WriteString(w, msg)
}

func copyFromFileHandler(w http.ResponseWriter, r *http.Request) {
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

	msg := fmt.Sprintf("{\"copyFromFile\":{\"dstaddr\":\"%s\"}}", vars["dstaddr"])
	io.WriteString(w, msg)
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

	if *srcInputFile == "" && *srcVaultAddr == "" {
		err = fmt.Errorf("Error: srcInputFile or and srcVaultAddr must be defined")
		return out, err
	}

	if *srcInputFile != "" && *doCopy == false && *doMirror == false {
		err = fmt.Errorf("Error: srcInputFile must be specified together with either doCopy or doMirror")
		return out, err
	}

	if *listenPort < 0 {
		err = fmt.Errorf("Error: Illegal listenPort value. It must be > 0 to act as a server listening on this port")
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

	if *srcVaultAddr != "" {
		srcClients = make([]*api.Client, *numWorkers)
	}

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
		r := mux.NewRouter()
		r.HandleFunc("/health", healthHandler).Methods(http.MethodGet)
		r.HandleFunc("/list/{srcaddr}", listHandler).Methods(http.MethodGet)
		r.HandleFunc("/copyfromfile/{dstaddr}/", copyFromFileHandler).Methods(http.MethodPost)
		r.HandleFunc("/copyfromvault/{srcaddr}/{dstaddr}", copyFromVaultHandler).Methods(http.MethodPost)
		addr  := fmt.Sprintf("localhost:%d", *listenPort)
		log.Fatal(http.ListenAndServe(addr, r))
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
