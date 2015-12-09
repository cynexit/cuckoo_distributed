package lib

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type CritsConn struct {
	C    *Core
	URL  string
	Data *CritsData
}

type CrtDefaultResponse struct {
	ReturnCode int    `json:"return_code"`
	ErrorMsg   string `json:"error_message"`
	Message    string `json:"message"`
	Id         string `json:"id"`
	Type       string `json:"type"`
}

type CrtResult struct {
	Subtype string
	Result  string
	Data    map[string]interface{}
}

func (c *Core) NewCrits(Data *CritsData) *CritsConn {
	return &CritsConn{
		C:    c,
		URL:  Data.CritsURL,
		Data: Data,
	}
}

func (crt *CritsConn) Log(level, msg string) error {
	data := url.Values{}
	data.Add("log_level", level)
	data.Add("log_message", msg)
	data.Add("analysis_id", crt.Data.AnalysisId)
	data.Add("object_type", crt.Data.ObjectType)
	data.Add("object_id", crt.Data.ObjectId)
	data.Add("username", crt.Data.Username)
	data.Add("api_key", crt.Data.ApiKey)

	r := &CrtDefaultResponse{}
	resp, status, err := crt.C.FastPostForm(crt.URL, data, r)

	if err != nil {
		return err
	}

	if status != 200 || r.ReturnCode != 0 || r.ErrorMsg != "" {
		return errors.New(fmt.Sprintf("%d - %s", status, resp))
	}

	return nil
}

// NewSample uploads the given file to crits.
func (crt *CritsConn) NewSample(fileData []byte, fileName string) (string, error) {
	crt.C.Debug.Printf("Uploading %s to crits [%s]\n", fileName, crt.Data.AnalysisId)

	// don't upload empty files (crits won't accept them)
	if len(fileData) == 0 {
		crt.Log("info", "Empty dropped file: "+fileName)
		return "", errors.New("empty file")
	}

	// add the file to the request
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("filedata", fileName)
	if err != nil {
		return "", err
	}
	part.Write(fileData)

	payload := make(map[string]string)
	payload["username"] = crt.Data.Username
	payload["api_key"] = crt.Data.ApiKey
	payload["source"] = crt.Data.Source
	payload["upload_type"] = "file"
	payload["file_format"] = "raw"

	// "auto-relationships" seem to be discarded
	//payload["related_md5"] = m.CritsData.MD5
	//payload["related_id"] = m.CritsData.ObjectId
	//payload["related_type"] = "sample"

	for key, val := range payload {
		err = writer.WriteField(key, val)
		if err != nil {
			return "", err
		}
	}

	err = writer.Close()
	if err != nil {
		return "", err
	}

	request, err := http.NewRequest("POST", crt.URL+"/api/v1/samples/", body)
	if err != nil {
		return "", err
	}
	request.Header.Add("Content-Type", writer.FormDataContentType())

	critsResp, err := crt.C.Client.Do(request)
	if err != nil {
		return "", err
	}
	defer SafeResponseClose(critsResp)

	respBody, err := ioutil.ReadAll(critsResp.Body)
	if err != nil {
		return "", err
	}

	r := &CrtDefaultResponse{}
	err = json.Unmarshal(respBody, r)
	if err != nil {
		return "", err
	}

	if r.ReturnCode != 0 || r.ErrorMsg != "" {
		return "", errors.New(string(respBody))
	}

	return r.Id, nil
}

// ForgeRelationship creates a relationship betwenn the object
// of the current CritsConn context and the supplied id.
func (crt *CritsConn) ForgeRelationship(id string) error {
	crt.C.Debug.Printf("Forging relationship with %s and [%s]\n", id, crt.Data.AnalysisId)

	data := url.Values{}
	data.Add("action", "forge_relationship")
	data.Add("right_type", "Sample")
	data.Add("right_id", id)
	data.Add("rel_type", "Related To")

	// There is currently a "bug" in the way crits parses the auth info
	// on a PATCH req so we are adding these two as HTTP header.
	//data.Add("username", m.CritsData.Username)
	//data.Add("api_key", m.CritsData.ApiKey)

	request, err := http.NewRequest(
		"PATCH",
		fmt.Sprintf("%s/api/v1/samples/%s/", crt.URL, crt.Data.ObjectId),
		bytes.NewBufferString(data.Encode()),
	)
	if err != nil {
		return err
	}

	request.Header.Add("Authorization", fmt.Sprintf("ApiKey %s:%s", crt.Data.Username, crt.Data.ApiKey))
	request.Header.Set("Content-Length", strconv.Itoa(len(data.Encode())))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	critsResp, err := crt.C.Client.Do(request)
	if err != nil {
		return err
	}
	defer SafeResponseClose(critsResp)

	respBody, err := ioutil.ReadAll(critsResp.Body)
	if err != nil {
		return err
	}

	r := &CrtDefaultResponse{}
	err = json.Unmarshal(respBody, r)
	if err != nil {
		return errors.New(err.Error() + " : " + string(respBody))
	}

	if (r.ReturnCode != 0 && r.Message != "Relationship already exists") || r.ErrorMsg != "" {
		return errors.New(string(respBody))
	}

	return nil
}

// AddResults is a "semi wrapper" for crits self._add_result and
// simple sends a batch of results back to crits.
func (crt *CritsConn) AddResults(results []*CrtResult) error {
	start := time.Now()

	if results == nil || len(results) == 0 {
		return nil
	}

	var (
		arrResult        []string
		arrResultType    []string
		arrResultSubtype []string
	)

	for _, r := range results {
		result_type := "{}"
		if r.Data != nil {
			// since crits can't handel bool values because
			// of ast_evalin the result_type map we have to
			// make sure to convert them beforehand.
			for k, v := range r.Data {
				if maybeBool, check := v.(bool); check {
					r.Data[k] = strconv.FormatBool(maybeBool)
				}
			}

			rtJson, err := json.Marshal(r.Data)
			if err != nil {
				return err
			}

			result_type = string(rtJson)
		}

		arrResult = append(arrResult, r.Result)
		arrResultType = append(arrResultType, result_type)
		arrResultSubtype = append(arrResultSubtype, r.Subtype)
	}

	arrResultJ, err := json.Marshal(arrResult)
	if err != nil {
		return err
	}

	arrResultTypeJ, err := json.Marshal(arrResultType)
	if err != nil {
		return err
	}

	arrResultSubtypeJ, err := json.Marshal(arrResultSubtype)
	if err != nil {
		return err
	}

	data := url.Values{}
	data.Add("result_is_batch", "true")
	data.Add("result", string(arrResultJ))
	data.Add("result_subtype", string(arrResultSubtypeJ))
	data.Add("result_type", string(arrResultTypeJ))
	data.Add("analysis_id", crt.Data.AnalysisId)
	data.Add("object_type", crt.Data.ObjectType)
	data.Add("object_id", crt.Data.ObjectId)
	data.Add("username", crt.Data.Username)
	data.Add("api_key", crt.Data.ApiKey)

	r := &CrtDefaultResponse{}
	resp, status, err := crt.C.FastPostForm(crt.URL+"/api/v1/services/", data, r)

	if err != nil {
		return err
	}

	if status != 200 || r.ReturnCode != 0 || r.ErrorMsg != "" {
		return errors.New(fmt.Sprintf("%d - %s", status, resp))
	}

	elapsed := time.Since(start)
	crt.C.Debug.Printf("Added %d results in %s to [%s]\n", len(results), elapsed, crt.Data.AnalysisId)

	return nil
}

// MarkAsRunnig does exactly what you'd expect, except
// that this does not work in crits so it does nothing
// currently.
func (crt *CritsConn) MarkAsRunning() error {
	// THIS IS NOT WORKING IN CRITS CURRENTLY!
	data := url.Values{}
	data.Add("finish", "0")
	data.Add("analysis_id", crt.Data.AnalysisId)
	data.Add("object_type", crt.Data.ObjectType)
	data.Add("object_id", crt.Data.ObjectId)
	data.Add("username", crt.Data.Username)
	data.Add("api_key", crt.Data.ApiKey)

	r := &CrtDefaultResponse{}
	resp, status, err := crt.C.FastPostForm(crt.URL+"/api/v1/services/", data, r)

	if err != nil {
		return err
	}

	if status != 200 || r.ReturnCode != 0 || r.ErrorMsg != "" {
		return errors.New(fmt.Sprintf("%d - %s", status, resp))
	}

	return nil
}

// MarkAsRunnig does exactly what you'd expect.
func (crt *CritsConn) MarkAsFinished() error {
	data := url.Values{}
	data.Add("finish", "1")
	data.Add("analysis_id", crt.Data.AnalysisId)
	data.Add("object_type", crt.Data.ObjectType)
	data.Add("object_id", crt.Data.ObjectId)
	data.Add("username", crt.Data.Username)
	data.Add("api_key", crt.Data.ApiKey)

	r := &CrtDefaultResponse{}
	resp, status, err := crt.C.FastPostForm(crt.URL+"/api/v1/services/", data, r)

	if err != nil {
		return err
	}

	if status != 200 || r.ReturnCode != 0 || r.ErrorMsg != "" {
		return errors.New(fmt.Sprintf("%d - %s", status, resp))
	}

	return nil
}
