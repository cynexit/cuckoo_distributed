package lib

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"time"
)

type CuckooConn struct {
	C   *Core
	URL string
}

type CkoStatus struct {
	Tasks     *CkoStatusTasks     `json:"tasks"`
	Diskspace *CkoStatusDiskspace `json:"diskspace"`
}

type CkoStatusTasks struct {
	Running int `json:"running"`
	Pending int `json:"pending"`
}

type CkoStatusDiskspace struct {
	Analyses *CkoStatusSamples `json:"samples"`
}

type CkoStatusSamples struct {
	Total int `json:"total"`
	Free  int `json:"free"`
	Used  int `json:"used"`
}

type CkoTasksCreateResp struct {
	TaskId int `json:"task_id"`
}

type CkoTasksViewResp struct {
	Task *CkoTasksViewTask
}

type CkoTasksViewTask struct {
	Status string `json:"status"`
}

type CkoTasksReport struct {
	Info       *CkoTasksReportInfo        `json:"info"`
	Signatures []*CkoTasksReportSignature `json;"signatures"`
	Behavior   *CkoTasksReportBehavior    `json:"behavior"`
}

type CkoTasksReportInfo struct {
	Started string          `json:"started"`
	Ended   string          `json:"ended"`
	Id      int             `json:"id"`
	Machine json.RawMessage `json:"machine"` //can be CkoTasksReportInfoMachine OR string
}

type CkoTasksReportInfoMachine struct {
	Name string `json:"name"`
}

type CkoTasksReportSignature struct {
	Severity    int    `json:"severity"`
	Description string `json:"description"`
	Name        string `json:"name"`
}

type CkoTasksReportBehavior struct {
	Processes []*CkoTasksReportBhvPcs   `json:"processes"`
	Summary   *CkoTasksReportBhvSummary `json:"summary"`
}

type CkoTasksReportBhvPcs struct {
	Name      string                      `json:"process_name"`
	Id        int                         `json:"process_id"`
	ParentId  int                         `json:"parent_id"`
	FirstSeen string                      `json:"first_seen"`
	Calls     []*CkoTasksReportBhvPcsCall `json:"calls"`
}

type CkoTasksReportBhvPcsCall struct {
	Category  string                         `json:"category"`
	Status    bool                           `json:"status"`
	Return    string                         `json:"return"`
	Timestamp string                         `json:"timestamp"`
	ThreadId  string                         `json:"thread_id"`
	Repeated  int                            `json:"repeated"`
	Api       string                         `json:"api"`
	Arguments []*CkoTasksReportBhvPcsCallArg `json:"arguments"`
	Id        int                            `json:"id"`
}

type CkoTasksReportBhvPcsCallArg struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type CkoTasksReportBhvSummary struct {
	Files   []string `json:"files"`
	Keys    []string `json:"keys"`
	Mutexes []string `json:"mutexes"`
}

type CkoFilesView struct {
	Sample *CkoFilesViewSample `json:"sample"`
}

type CkoFilesViewSample struct {
	SHA1     string `json:"sha1"`
	FileType string `json:"file_type"`
	FileSize int    `json:"file_size"`
	CRC32    string `json:"crc32"`
	SSDeep   string `json:"ssdeep"`
	SHA256   string `json:"sha256"`
	SHA512   string `json:"sha512"`
	Id       int    `json:"id"`
	MD5      string `json:"md5"`
}

func (c *Core) NewCuckoo(URL string) *CuckooConn {
	return &CuckooConn{
		C:   c,
		URL: URL,
	}
}

func (cko *CuckooConn) GetPending() (int, error) {
	r := &CkoStatus{}
	resp, status, err := cko.C.FastGet(cko.URL+"/cuckoo/status", r)
	if err != nil || status != 200 {
		if resp != nil {
			err = errors.New(fmt.Sprintf("%s -> [%d] %s", err.Error(), status, resp))
		}

		return 0, err
	}

	return r.Tasks.Pending, nil
}

func (cko *CuckooConn) GetStatus() (*CkoStatus, error) {
	r := &CkoStatus{}
	resp, status, err := cko.C.FastGet(cko.URL+"/cuckoo/status", r)
	if err != nil || status != 200 {
		if resp != nil {
			err = errors.New(fmt.Sprintf("%s -> [%d] %s", err.Error(), status, resp))
		}

		return nil, err
	}

	return r, nil
}

func (cko *CuckooConn) GetFileInfoByMD5(md5 string) (*CkoFilesViewSample, error) {
	r := &CkoFilesView{}
	resp, status, err := cko.C.FastGet(cko.URL+"/files/view/md5/"+md5, r)
	if err != nil || status != 200 {
		if resp != nil {
			err = errors.New(fmt.Sprintf("%s -> [%d] %s", err.Error(), status, resp))
		}

		return nil, err
	}

	return r.Sample, nil
}

// submitTask submits a new task to the cuckoo api.
func (cko *CuckooConn) NewTask(fileBytes []byte, fileName string, params map[string]string) (int, error) {
	start := time.Now()

	// add the file to the request
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return 0, err
	}
	part.Write(fileBytes)

	// add the extra payload to the request
	for key, val := range params {
		err = writer.WriteField(key, val)
		if err != nil {
			return 0, err
		}
	}

	err = writer.Close()
	if err != nil {
		return 0, err
	}

	// finalize request
	request, err := http.NewRequest("POST", cko.URL+"/tasks/create/file", body)
	if err != nil {
		return 0, err
	}
	request.Header.Add("Content-Type", writer.FormDataContentType())

	// perform request
	resp, err := cko.C.Client.Do(request)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, errors.New(resp.Status)
	}

	// parse response
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	r := &CkoTasksCreateResp{}
	if err := json.Unmarshal(respBody, r); err != nil {
		return 0, err
	}

	elapsed := time.Since(start)
	cko.C.Debug.Printf("Uploaded %s to cuckoo in %s\n", fileName, elapsed)

	return r.TaskId, nil
}

func (cko *CuckooConn) TaskStatus(id int) (string, error) {
	r := &CkoTasksViewResp{}
	resp, status, err := cko.C.FastGet(fmt.Sprintf("%s/tasks/view/%d", cko.URL, id), r)
	if err != nil || status != 200 {
		if resp != nil {
			err = errors.New(fmt.Sprintf("%s -> [%d] %s", err.Error(), status, resp))
		}

		return "", err
	}

	return r.Task.Status, nil
}

func (cko *CuckooConn) TaskReport(id int) (*CkoTasksReport, error) {
	start := time.Now()
	r := &CkoTasksReport{}
	resp, status, err := cko.C.FastGet(fmt.Sprintf("%s/tasks/report/%d", cko.URL, id), r)
	if err != nil || status != 200 {
		if resp != nil {
			err = errors.New(fmt.Sprintf("%s -> [%d] %s", err.Error(), status, resp))
		}

		return nil, err
	}

	elapsed := time.Since(start)
	cko.C.Debug.Printf("Downloaded report %d from cuckoo in %s\n", id, elapsed)

	return r, nil
}

func (cko *CuckooConn) DeleteTask(id int) error {
	resp, status, err := cko.C.FastGet(fmt.Sprintf("%s/tasks/delete/%d", cko.URL, id), nil)
	if err != nil {
		if resp != nil {
			err = errors.New(fmt.Sprintf("%s -> [%d] %s", err.Error(), status, resp))
		}

		return err
	}

	if status != 200 {
		return errors.New(fmt.Sprintf("%d - Response code not 200", status))
	}

	return nil
}

func (cko *CuckooConn) GetDropped(id int) ([]byte, error) {
	start := time.Now()
	resp, status, err := cko.C.FastGet(fmt.Sprintf("%s/tasks/report/%d/dropped", cko.URL, id), nil)
	if err != nil || status != 200 {
		if resp != nil {
			err = errors.New(fmt.Sprintf("%s -> [%d] %s", err.Error(), status, resp))
		}

		return []byte{}, err
	}

	elapsed := time.Since(start)
	cko.C.Debug.Printf("Downloaded dropped files %d from cuckoo in %s\n", id, elapsed)

	return resp, nil
}
