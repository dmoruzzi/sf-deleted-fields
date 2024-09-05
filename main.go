package main

import (
	"crypto/md5"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

//go:embed soql/*.soql
var queries embed.FS

type DeleteCountRecord struct {
	DeveloperName    string `json:"DeveloperName"`
	TableEnumOrId    string `json:"TableEnumOrId"`
	QualifiedApiName string `json:"QualifiedApiName"`
	ApiName          string `json:"ApiName"`
	Count            int    `json:"Count"`
	Timestamp        int64  `json:"Timestamp"`
}

type LastCount struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

type ExportData struct {
	Results      []DeleteCountRecord `json:"results"`
	LastRunCount []LastCount         `json:"lastRunCount"`
}

var (
	deleteCounts []DeleteCountRecord
	mu           sync.Mutex
)

func main() {
	org := flag.String("org", "", "Salesforce organization to use")
	export := flag.String("export", "deleted_fields.json", "File to export the results as JSON")
	flag.Parse()

	if *org == "" {
		log.Fatal("[ERROR] Please provide a Salesforce organization alias; use --org")
		os.Exit(1)
	}

	log.Printf("[DEBUG] Using Salesforce organization: %s", *org)

	sfCliInstallCheck()

	log.Println("[DEBUG] Querying deleted fields data")
	deletedFieldsCSV, err := queryFieldData(*org, "soql/deleted_fields.soql", "", true)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("[TRACE] Deleted fields data:\n\t", strings.ReplaceAll(deletedFieldsCSV, "\n", "\n\t"))

	log.Println("[DEBUG] Processing deleted fields data")
	processDeletedFields(deletedFieldsCSV, *org)

	if *export != "" {
		log.Printf("[DEBUG] Exporting results to %s", *export)
		exportResultsAsJSON(*export)
	}
}

func calculateMD5(file *os.File) (string, error) {
	hasher := md5.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", fmt.Errorf("failed to calculate MD5 hash: %w", err)
	}

	md5Hash := hex.EncodeToString(hasher.Sum(nil))
	return md5Hash, nil
}

func sfCliInstallCheck() {
	log.Println("[DEBUG] Checking Salesforce CLI installation")
	cmd := exec.Command("sf", "version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Fatal("[ERROR] sf is not installed:", err)
	}

	for _, line := range strings.Split(string(output), "\n") {
		if line != "" && !strings.Contains(line, "Warning:") {
			log.Println("[DEBUG] Salesforce CLI Version:", line)
		}
	}
}

func queryFieldData(sfOrg, queryFile, queryId string, useToolingApi bool) (string, error) {
	log.Printf("[DEBUG] Reading query file: %s", queryFile)

	queryData, err := queries.ReadFile(queryFile)
	if err != nil {
		return "", fmt.Errorf("query file read failed: %w", err)
	}

	queryDataStr := strings.ReplaceAll(string(queryData), "\n", " ")
	if queryId != "" {
		queryDataStr = strings.ReplaceAll(queryDataStr, "#", queryId)
	}

	cmdArgs := []string{"data", "query", "-o", sfOrg, "-r", "csv", "-q", queryDataStr}
	if useToolingApi {
		cmdArgs = append(cmdArgs, "-t")
	}
	log.Printf("[DEBUG] Executing query [Tooling API: %t]: %v", useToolingApi, cmdArgs)

	cmd := exec.Command("sf", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("command execution failed: %w\nOUTPUT: %s", err, string(output))
	}

	return extractCSVData(output), nil
}

func extractCSVData(output []byte) string {
	log.Println("[DEBUG] Extracting CSV data from query output")
	var csvData strings.Builder
	processingCSV := false
	for _, line := range strings.Split(string(output), "\n") {
		if processingCSV {
			if line != "" {
				csvData.WriteString(line + "\n")
				log.Println("[DEBUG] CSV data:", line)
			}
		} else if strings.Contains(line, ",") {
			processingCSV = true
			csvData.WriteString(line + "\n")
			log.Println("[DEBUG] CSV data:", line)
		}
	}
	return csvData.String()
}

func processDeletedFields(deletedFieldsCSV, org string) {
	var wg sync.WaitGroup

	for _, line := range strings.Split(deletedFieldsCSV, "\n") {
		if line == "" || strings.HasPrefix(line, "DeveloperName") {
			log.Printf("[DEBUG] Skipping line: %s", line)
			continue
		}

		data := strings.Split(line, ",")
		if !strings.HasSuffix(data[0], "_del") {
			log.Printf("[DEBUG] Skipping non-deleted field: DeveloperName=%s, TableEnumOrId=%s", data[0], data[1])
			continue // Skip non-deleted fields
		}

		wg.Add(1)
		go func(data []string) {
			defer wg.Done()

			if strings.HasPrefix(data[1], "01I") {
				log.Printf("[DEBUG] Processing deleted field: DeveloperName=%s, TableEnumOrId=%s", data[0], data[1])
				devNameCSV, err := queryFieldData(org, "soql/enum_to_developer_name.soql", data[1], true)
				if err != nil {
					log.Fatal(err)
				}

				processDeveloperNames(devNameCSV, data[0], data[1], org)
			} else {
				log.Printf("[DEBUG] Processing deleted field: DeveloperName=%s, TableEnumOrId=%s", data[0], data[1])
				devNameCSV := data[0] + ",DeveloperName\n" + data[0] + "," + data[0]

				processDeveloperNames(devNameCSV, data[0], data[1], org)
			}
		}(data)
	}

	wg.Wait()
}

func processDeveloperNames(devNameCSV, developerName, tableEnumOrId, org string) {
	var wg sync.WaitGroup

	for _, devLine := range strings.Split(devNameCSV, "\n") {
		if devLine == "" {
			log.Printf("[DEBUG] Skipping line: %s", devLine)
			continue
		}

		apiData := strings.Split(devLine, ",")
		if apiData[1] == "DeveloperName" {
			log.Printf("[DEBUG] Skipping line: %s", devLine)
			continue
		}

		wg.Add(1)
		go func(apiData []string) {
			defer wg.Done()

			log.Printf("[DEBUG] Processing developer name: DeveloperName=%s, API Name=%s", developerName, apiData[1])
			apiNameCSV, err := queryFieldData(org, "soql/developer_name_to_api_name.soql", apiData[1], false)
			if err != nil {
				log.Fatal(err)
			}

			processApiNames(apiNameCSV, developerName, tableEnumOrId, apiData[1], org)
		}(apiData)
	}

	wg.Wait()
}

func processApiNames(apiNameCSV, developerName, tableEnumOrId, apiName, org string) {
	var wg sync.WaitGroup

	for _, apiLine := range strings.Split(apiNameCSV, "\n") {
		if apiLine == "" {
			continue
		}

		apiData := strings.Split(apiLine, ",")
		if apiData[2] == "QualifiedApiName" {
			continue
		}

		wg.Add(1)
		go func(apiData []string) {
			defer wg.Done()

			log.Printf("[DEBUG] Processing API name: QualifiedApiName=%s", apiData[2])
			query := fmt.Sprintf("SELECT Count() FROM %s", apiData[2])
			cmdArgs := []string{"data", "query", "-q", query, "-o", org, "-r", "json"}

			count, err := queryCount(cmdArgs)
			if err != nil {
				log.Fatal(err)
			}

			timestamp := time.Now().Unix()
			record := DeleteCountRecord{
				DeveloperName:    developerName,
				TableEnumOrId:    tableEnumOrId,
				QualifiedApiName: apiData[2],
				ApiName:          apiData[1],
				Count:            count,
				Timestamp:        timestamp,
			}

			log.Printf("[DEBUG] Appending delete count record: %+v", record)

			mu.Lock()
			deleteCounts = append(deleteCounts, record)
			mu.Unlock()
		}(apiData)
	}

	wg.Wait()
}

func queryCount(cmdArgs []string) (int, error) {
	log.Printf("[DEBUG] Querying count with args: %v", cmdArgs)
	cmd := exec.Command("sf", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("command execution failed: %w\nOUTPUT: %s", err, string(output))
	}

	output = skipFirstLineIfNeeded(output)
	var jsonData map[string]interface{}
	if err := json.Unmarshal(output, &jsonData); err != nil {
		return 0, fmt.Errorf("JSON Unmarshal failed: %w\nOUTPUT: %s", err, string(output))
	}

	result, ok := jsonData["result"].(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("'result' field is not a map")
	}

	totalSize, ok := result["totalSize"].(float64)
	if !ok {
		return 0, fmt.Errorf("'totalSize' field is not a float64")
	}

	return int(totalSize), nil
}

func skipFirstLineIfNeeded(output []byte) []byte {
	outputStr := string(output)
	if strings.Contains(outputStr, "»") || strings.Contains(outputStr, "update available") {
		if index := strings.Index(outputStr, "\n"); index != -1 {
			return []byte(outputStr[index+1:])
		}
	}
	return output
}

func exportResultsAsJSON(filename string) {
	log.Printf("[DEBUG] Exporting results to JSON file: %s", filename)
	var exportData ExportData

	if _, err := os.Stat(filename); err == nil {
		log.Printf("[DEBUG] Reading existing data from file: %s", filename)
		file, err := os.Open(filename)
		if err != nil {
			log.Fatalf("Failed to open existing file: %s", err)
		}
		defer file.Close()

		decoder := json.NewDecoder(file)
		if err := decoder.Decode(&exportData); err != nil {
			log.Fatalf("Failed to decode existing JSON data: %s", err)
		}
	}

	exportData.Results = append(exportData.Results, deleteCounts...)
	exportData.LastRunCount = calculateCurCounts(deleteCounts)

	file, err := os.Create(filename)
	if err != nil {
		log.Fatalf("Failed to create file: %s", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(exportData); err != nil {
		log.Fatalf("Failed to encode JSON: %s", err)
	}

	md5Hash, err := calculateMD5(file)
	if err != nil {
		log.Fatalf("Failed to calculate MD5 hash: %s", err)
	}

	log.Printf("[INFO] Successfully exported results to JSON file: %s with MD5 hash: %s", filename, md5Hash)
}

func calculateCurCounts(records []DeleteCountRecord) []LastCount {
	log.Println("[DEBUG] Calculating current counts from records")

	if len(records) == 0 {
		todayDate := time.Now().Format("2006-01-02")
		log.Printf("[INFO] No records found, setting count to 0 and date to %s", todayDate)
		return []LastCount{
			{
				Date:  todayDate,
				Count: 0,
			},
		}
	}

	counts := make(map[string]int)
	processed := make(map[string]map[string]bool) // QualifiedApiName -> Date

	for _, record := range records {
		date := time.Unix(record.Timestamp, 0).Format("2006-01-02")

		if _, exists := processed[date]; !exists {
			processed[date] = make(map[string]bool)
		}

		if !processed[date][record.QualifiedApiName] {
			counts[date] += record.Count
			processed[date][record.QualifiedApiName] = true
		}
	}

	var curCounts []LastCount
	for date, count := range counts {
		log.Printf("[INFO] Count for date %s: %d", date, count)
		curCounts = append(curCounts, LastCount{
			Date:  date,
			Count: count,
		})
	}

	return curCounts
}
