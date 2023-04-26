/*
* Copyright (c) 2015-2020 by MemSQL. All rights reserved.
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*
*     http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
 */

package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/awreece/goini"
)

type Config struct {
	Flavor         DatabaseFlavor
	Duration       time.Duration
	Setup          []string
	Teardown       []string
	Jobs           map[string]*Job
	AcceptedErrors Set
}

func (c *Config) String() string {
	return quotedStruct(c)
}

func readQueriesFromReader(df DatabaseFlavor, r io.Reader) ([]string, error) {
	queries := make([]string, 0, 1)
	if contents, err := ioutil.ReadAll(r); err != nil {
		return nil, err
	} else {
		for _, query := range strings.Split(string(contents), df.QuerySeparator()) {
			err := df.CheckQuery(query)
			if err != nil && err != EmptyQueryError {
				return nil, fmt.Errorf("invalid query %v", err)
			} else if err == nil {
				queries = append(queries, query)
			}
		}
	}
	return queries, nil
}

func readQueriesFromFile(df DatabaseFlavor, queryFile string) ([]string, error) {
	file, err := os.Open(queryFile)
	if err != nil {
		return nil, err
	}
	return readQueriesFromReader(df, file)
}

type globalSectionParser struct {
	config *Config
	flavor DatabaseFlavor
}

var globalOptions = goini.DecodeOptionSet{
	"duration": &goini.DecodeOption{Kind: goini.UniqueOption,
		Usage: "When the test will stop launching new jobs, as a duration " +
			" elapsed since setup ",
		Parse: func(v string, gsp interface{}) (e error) {
			gsp.(*globalSectionParser).config.Duration, e = time.ParseDuration(v)
			return e
		},
	},
	"error": &goini.DecodeOption{Kind: goini.MultiOption,
		Usage: "Globally accepted errors.",
		Parse: func(v string, gspi interface{}) error {
			gsp := gspi.(*globalSectionParser)
			if gsp.config.AcceptedErrors == nil {
				gsp.config.AcceptedErrors = make(Set)
			}
			gsp.config.AcceptedErrors.Add(v)
			return nil
		},
	},
}

func decodeGlobalSection(df DatabaseFlavor, s goini.RawSection, c *Config) error {
	return globalOptions.Decode(s, &globalSectionParser{c, df})
}

func validateGlobalSection(jsonConfig JSONConfig, c *Config) (err error) {
	v := reflect.ValueOf(jsonConfig)

	if isFieldSet(v, "Duration") {
		if c.Duration, err = time.ParseDuration(jsonConfig.Duration); err != nil {
			return err
		}
	}
	if isFieldSet(v, "Errors") {
		errors := jsonConfig.Errors
		c.AcceptedErrors = make(Set)

		for _, e := range errors {
			c.AcceptedErrors.Add(e)
		}
	}

	return nil
}

type setupSectionParser struct {
	queries []string
	df      DatabaseFlavor
	basedir string
}

var setupOptions = goini.DecodeOptionSet{
	"query": &goini.DecodeOption{Kind: goini.MultiOption,
		Usage: "Setup query to be executed before any jobs are started. " +
			"Must be a single query and cannot have any effect on the " +
			"connection (e.g USE or BEGIN).",
		Parse: func(v string, sspi interface{}) error {
			ssp := sspi.(*setupSectionParser)
			if e := ssp.df.CheckQuery(v); e != nil {
				return e
			}
			ssp.queries = append(ssp.queries, v)
			return nil
		},
	},
	"query-file": &goini.DecodeOption{Kind: goini.MultiOption,
		Usage: "Setup query to be executed before any jobs are started. " +
			"Must be a single query and cannot have any effect on the " +
			"connection (e.g USE or BEGIN).",
		Parse: func(v string, sspi interface{}) error {
			ssp := sspi.(*setupSectionParser)
			if !filepath.IsAbs(v) {
				v = filepath.Join(ssp.basedir, v)
			}
			if qs, err := readQueriesFromFile(ssp.df, v); err != nil {
				return err
			} else {
				ssp.queries = append(ssp.queries, qs...)
				return nil
			}
		},
	},
}

func decodeSetupSection(df DatabaseFlavor, s goini.RawSection, basedir string, ss *[]string) error {
	parser := setupSectionParser{df: df, basedir: basedir}
	err := setupOptions.Decode(s, &parser)
	if err == nil {
		*ss = parser.queries
	}
	return err
}

type ReservedSectionOptions struct {
	Queries     []string `json:"queries,omitempty"`
	QueryFiles  []string `json:"queryFiles,omitempty"`
}

func validateReservedSection(df DatabaseFlavor, jsonConfig JSONConfig, basedir string, sectionName string, ss *[]string) (err error) {
	v := reflect.ValueOf(jsonConfig)

	if !isFieldSet(v, sectionName) {
		return nil
	} 

	var section ReservedSectionOptions
	switch sectionName {
	case "Setup":
		section = jsonConfig.Setup
	case "Teardown":
		section = jsonConfig.Teardown
	}
	v = reflect.ValueOf(section)

	if isFieldSet(v, "Queries") {
		queries := section.Queries

		for _, query := range queries {
			if err := df.CheckQuery(query); err != nil {
				return err
			}

			*ss = append(*ss, query)
		}
	}
	if isFieldSet(v, "QueryFiles") {
		queryFiles := section.QueryFiles

		for _, queryFile := range queryFiles {
			if !filepath.IsAbs(queryFile) {
				queryFile = filepath.Join(basedir, queryFile)
			}
	
			if queries, err := readQueriesFromFile(df, queryFile); err != nil {
				return err
			} else {
				*ss = append(*ss, queries...)
			}
		}
	}

	return nil
}

type jobParser struct {
	j                 *Job
	df                DatabaseFlavor
	basedir           string
	queryArgsFile     io.Reader
	queryArgsDelim    rune
	multiQueryAllowed bool
}

var jobOptions = goini.DecodeOptionSet{
	"start": &goini.DecodeOption{Kind: goini.UniqueOption,
		Usage: "When this job should start, as a duration elapsed since setup.",
		Parse: func(v string, jp interface{}) (e error) {
			jp.(*jobParser).j.Start, e = time.ParseDuration(v)
			return e
		},
	},
	"stop": &goini.DecodeOption{Kind: goini.UniqueOption,
		Usage: "When this job should stop, as a duration elapsed since setup.",
		Parse: func(v string, jp interface{}) (e error) {
			jp.(*jobParser).j.Stop, e = time.ParseDuration(v)
			return e
		},
	},
	"query": &goini.DecodeOption{Kind: goini.MultiOption,
		Usage: "Query to execute for the job. " +
			"Must be a single query and cannot have any effect on the " +
			"connection (e.g USE or BEGIN).",
		Parse: func(v string, jpi interface{}) error {
			jp := jpi.(*jobParser)
			if e := jp.df.CheckQuery(v); e != nil {
				return e
			} else {
				jp.j.Queries = append(jp.j.Queries, v)
				return nil
			}
		},
	},
	"query-file": &goini.DecodeOption{Kind: goini.MultiOption,
		Usage: "File containing queries to execute for the job. " +
			"Queries are separated by the query-separator and cannot have any " +
			"effect on the connection (e.g USE or BEGIN).",
		Parse: func(v string, jpi interface{}) error {
			jp := jpi.(*jobParser)
			if !filepath.IsAbs(v) {
				v = filepath.Join(jp.basedir, v)
			}
			if qs, err := readQueriesFromFile(jp.df, v); err != nil {
				return err
			} else {
				jp.j.Queries = append(jp.j.Queries, qs...)
				return nil
			}
		},
	},
	"query-args-file": &goini.DecodeOption{Kind: goini.UniqueOption,
		Usage: "File containing csv delimited query args, one line per " +
			"query.",
		Parse: func(v string, jpi interface{}) (err error) {
			jp := jpi.(*jobParser)
			if !filepath.IsAbs(v) {
				v = filepath.Join(jp.basedir, v)
			}
			jp.queryArgsFile, err = os.Open(v)
			return err
		},
	},
	"query-args-delim": &goini.DecodeOption{Kind: goini.UniqueOption,
		Usage: "Field separator for csv delimited query args.",
		Parse: func(v string, jpi interface{}) error {
			jp := jpi.(*jobParser)
			if s, err := strconv.Unquote(v); err != nil {
				return err
			} else if len(s) != 1 {
				return errors.New("Must provide exactly one character for delimiter")
			} else {
				jp.queryArgsDelim, _ = utf8.DecodeRuneInString(s)
				return nil
			}
		},
	},
	"query-results-file": &goini.DecodeOption{Kind: goini.UniqueOption,
		Usage: "Results from executed queries will be written to this file " +
			"as comma separated values. If the file already exists, it " +
			"will be truncated",
		Parse: func(v string, jpi interface{}) (err error) {
			jp := jpi.(*jobParser)
			if !filepath.IsAbs(v) {
				v = filepath.Join(jp.basedir, v)
			}
			jp.j.QueryResults, err = NewSafeCSVWriter(v)
			return err
		},
	},
	"rate": &goini.DecodeOption{Kind: goini.UniqueOption,
		Usage: "The number of batches executed per second (default 0.0).",
		Parse: func(v string, jpi interface{}) (e error) {
			jp := jpi.(*jobParser)
			jp.j.Rate, e = strconv.ParseFloat(v, 64)
			if e == nil && jp.j.Rate < 0 {
				return errors.New("invalid negative value for rate")
			}
			return e
		},
	},
	"batch-size": &goini.DecodeOption{Kind: goini.UniqueOption,
		Usage: "Number of jobs started during one batch (default 1).",
		Parse: func(v string, jp interface{}) (e error) {
			jp.(*jobParser).j.BatchSize, e = strconv.ParseUint(v, 10, 0)
			return e
		},
	},
	"queue-depth": &goini.DecodeOption{Kind: goini.UniqueOption,
		Usage: "Number of simultaneous executions of the job allowed.",
		Parse: func(v string, jp interface{}) (e error) {
			// Is there a way to make go respect numeric prefixes (e.g. 0x0)?
			jp.(*jobParser).j.QueueDepth, e = strconv.ParseUint(v, 10, 0)
			return e
		},
	},
	"concurrency": &goini.DecodeOption{Kind: goini.UniqueOption,
		Usage: "Number of simultaneous executions of the job allowed.",
		Parse: func(v string, jp interface{}) (e error) {
			// Is there a way to make go respect numeric prefixes (e.g. 0x0)?
			jp.(*jobParser).j.QueueDepth, e = strconv.ParseUint(v, 10, 0)
			return e
		},
	},
	"count": &goini.DecodeOption{Kind: goini.UniqueOption,
		Usage: "Number of time job is executed before stopping.",
		Parse: func(v string, jp interface{}) (e error) {
			jp.(*jobParser).j.Count, e = strconv.ParseUint(v, 10, 0)
			return e
		},
	},
	"multi-query-mode": &goini.DecodeOption{Kind: goini.UniqueOption,
		Usage: "Set to 'multi-connection' to signal that the job will execute " +
			"multiple queries, but it is safe for them to be on different " +
			"connections.",
		Parse: func(v string, jp interface{}) error {
			if v == "multi-connection" {
				jp.(*jobParser).multiQueryAllowed = true
				return nil
			} else {
				return fmt.Errorf("invalid value for multi-query-mode: %s",
					strconv.Quote(v))
			}
		},
	},
	"query-log-file": &goini.DecodeOption{Kind: goini.UniqueOption,
		Usage: "A flat text file containing a log file to replay instead of a " +
			"normal job. The query log format is a series of newline " +
			"delimited records containing a time in microseconds and a query " +
			"separated by a comma. For example, '8644882534,select 1'.",
		Parse: func(v string, jpi interface{}) (e error) {
			jp := jpi.(*jobParser)
			if !filepath.IsAbs(v) {
				v = filepath.Join(jp.basedir, v)
			}
			jp.j.QueryLog, e = os.Open(v)
			return e
		},
	},
}

func decodeJobSection(df DatabaseFlavor, section goini.RawSection, basedir string, job *Job) error {
	jp := jobParser{j: job, df: df, basedir: basedir}

	if err := jobOptions.Decode(section, &jp); err != nil {
		return err
	} else if len(job.Queries) == 0 && job.QueryLog == nil {
		return errors.New("no query provided")
	} else if len(job.Queries) > 0 && job.QueryLog != nil {
		return errors.New("cannot have both queries and a query log")
	} else if len(job.Queries) > 1 && !jp.multiQueryAllowed {
		return fmt.Errorf("must have only one query")
	} else if job.Rate == 0 && job.BatchSize > 0 {
		return errors.New("can only specify batch-size with rate")
	} else if jp.queryArgsDelim != 0 && jp.queryArgsFile == nil {
		return errors.New("Cannot set query-args-delim with no query-args-file")
	} else if jp.queryArgsFile != nil && job.QueryLog != nil {
		return errors.New("Cannot use query-args-file with query-log-file")
	}

	differentJobTypes := 0
	if job.QueueDepth > 0 {
		differentJobTypes += 1
	}
	if job.QueryLog != nil {
		differentJobTypes += 1
	}
	if job.Rate > 0 {
		differentJobTypes += 1
	}
	// The default job type is 1 thread.
	if differentJobTypes == 0 {
		job.QueueDepth = 1
	}

	if differentJobTypes > 1 {
		return errors.New("Can only specify one of rate, queue-depth, or query-log-file")
	}

	if job.Rate > 0 && job.BatchSize == 0 {
		job.BatchSize = 1
	}

	if jp.queryArgsFile != nil {
		job.QueryArgs = csv.NewReader(jp.queryArgsFile)
		if jp.queryArgsDelim != 0 {
			job.QueryArgs.Comma = jp.queryArgsDelim
		}
	}

	return nil
}

func decodeConfigJobs(df DatabaseFlavor, iniConfig *goini.RawConfig, basedir string, config *Config) error {
	config.Jobs = make(map[string]*Job)
	for _, name := range iniConfig.Sections() {
		// Don't try to parse a reserved section as a job.
		if name == "setup" || name == "teardown" || name == "global" {
			continue
		}
		section := iniConfig.Section(name)

		job := new(Job)
		job.Name = name
		if err := decodeJobSection(df, section, basedir, job); err != nil {
			return fmt.Errorf("Error parsing job %s: %v",
				strconv.Quote(name), err)
		}
		config.Jobs[name] = job
	}
	return nil
}

type JobOptions struct {
	Start            string   `json:"start,omitempty"`
	Stop             string   `json:"stop,omitempty"`
	Queries          []string `json:"queries,omitempty"`
	QueryFiles       []string `json:"queryFiles,omitempty"`
	QueryArgsFile    string   `json:"queryArgsFile,omitempty"`
	QueryArgsDelim   string   `json:"queryArgsDelim,omitempty"`
	QueryResultsFile string   `json:"queryResultsFile,omitempty"`
	Rate             float64  `json:"rate,omitempty"`
	BatchSize        uint64   `json:"batchSize,omitempty"`
	QueueDepth       uint64   `json:"queueDepth,omitempty"`
	Concurrency      uint64   `json:"concurrency,omitempty"`
	Count            uint64   `json:"count,omitempty"`
	MultiQueryMode   bool     `json:"multiQueryMode,omitempty"`
	QueryLogFile     string   `json:"queryLogFile,omitempty"` 
}

func validateJobSection(df DatabaseFlavor, jobSpec JobOptions , basedir string, job *Job) (err error) {
	jp := jobParser{j: job, df: df, basedir: basedir}
	v := reflect.ValueOf(jobSpec)

	if isFieldSet(v, "Start") {
		if job.Start, err = time.ParseDuration(jobSpec.Start); err != nil {
			return err
		}
	}
	if isFieldSet(v, "Stop") {
		if job.Stop, err = time.ParseDuration(jobSpec.Stop); err != nil {
			return err
		}
	}
	if isFieldSet(v, "Queries") {
		queries := jobSpec.Queries

		for _, query := range queries {
			if err := df.CheckQuery(query); err != nil {
				return err
			}

			job.Queries = append(job.Queries, query)
		}
	}
	if isFieldSet(v, "QueryFiles") {
		queryFiles := jobSpec.QueryFiles

		for _, queryFile := range queryFiles {
			if !filepath.IsAbs(queryFile) {
				queryFile = filepath.Join(basedir, queryFile)
			}
	
			if queries, err := readQueriesFromFile(df, queryFile); err != nil {
				return err
			} else {
				job.Queries = append(job.Queries, queries...)
			}
		}
	}
	if isFieldSet(v, "QueryArgsFile") {
		queryArgsFile := jobSpec.QueryArgsFile
		if !filepath.IsAbs(queryArgsFile) {
			queryArgsFile = filepath.Join(basedir, queryArgsFile)
		}

		if jp.queryArgsFile, err = os.Open(queryArgsFile); err != nil {
			return err
		}
	}
	if isFieldSet(v, "QueryArgsDelim") {
		queryArgsDelim := jobSpec.QueryArgsDelim

		if len(queryArgsDelim) != 1 {
			return errors.New("Must provide exactly one character for delimiter")
		}

		jp.queryArgsDelim, _ = utf8.DecodeRuneInString(queryArgsDelim)
	}
	if isFieldSet(v, "QueryResultsFile") {
		queryResultsFile := jobSpec.QueryResultsFile
		if !filepath.IsAbs(queryResultsFile) {
			queryResultsFile = filepath.Join(basedir, queryResultsFile)
		}

		if job.QueryResults, err = NewSafeCSVWriter(queryResultsFile); err != nil {
			return err
		}
	}
	if isFieldSet(v, "Rate") {
		if jobSpec.Rate < 0 {
			return errors.New("invalid negative value for rate")
		}

		job.Rate = jobSpec.Rate
	}
	if isFieldSet(v, "BatchSize") {
		job.BatchSize = jobSpec.BatchSize
	}
	if isFieldSet(v, "QueueDepth") {
		job.QueueDepth = jobSpec.QueueDepth
	}
	if isFieldSet(v, "Concurrency") {
		job.QueueDepth = jobSpec.Concurrency
	}
	if isFieldSet(v, "Count") {
		job.Count = jobSpec.Count
	}
	if isFieldSet(v, "MultiQueryMode") {
		jp.multiQueryAllowed = jobSpec.MultiQueryMode
	}
	if isFieldSet(v, "QueryLogFile") {
		queryLogFile := jobSpec.QueryLogFile
		if !filepath.IsAbs(queryLogFile) {
			queryLogFile = filepath.Join(basedir, queryLogFile)
		}

		if job.QueryLog, err = os.Open(queryLogFile); err != nil {
			return err
		}
	}

	if len(job.Queries) == 0 && job.QueryLog == nil {
		return errors.New("no query provided")
	}
	if len(job.Queries) > 0 && job.QueryLog != nil {
		return errors.New("cannot have both queries and a queryLog")
	}
	if len(job.Queries) > 1 && !jp.multiQueryAllowed {
		return fmt.Errorf("must have only one query")
	}
	if job.Rate == 0 && job.BatchSize > 0 {
		return errors.New("can only specify batchSize with rate")
	}
	if jp.queryArgsDelim != 0 && jp.queryArgsFile == nil {
		return errors.New("Cannot set queryArgsDelim with no queryArgsFile")
	}
	if jp.queryArgsFile != nil && job.QueryLog != nil {
		return errors.New("Cannot use queryArgsFile with queryLogFile")
	}

	differentJobTypes := 0
	if job.QueueDepth > 0 {
		differentJobTypes += 1
	}
	if job.QueryLog != nil {
		differentJobTypes += 1
	}
	if job.Rate > 0 {
		differentJobTypes += 1
	}
	// The default job type is 1 thread.
	if differentJobTypes == 0 {
		job.QueueDepth = 1
	} else if differentJobTypes > 1 {
		return errors.New("Can only specify one of rate, queue-depth, or query-log-file")
	}

	if job.Rate > 0 && job.BatchSize == 0 {
		job.BatchSize = 1
	}

	if jp.queryArgsFile != nil {
		job.QueryArgs = csv.NewReader(jp.queryArgsFile)
		if jp.queryArgsDelim != 0 {
			job.QueryArgs.Comma = jp.queryArgsDelim
		}
	}

	return nil
}

func validateConfigJobs(df DatabaseFlavor, jsonConfig JSONConfig, basedir string, c *Config) (err error) {
	c.Jobs = make(map[string]*Job)

	v := reflect.ValueOf(jsonConfig)

	if !isFieldSet(v, "Jobs") {
		return nil
	}

	jobs := jsonConfig.Jobs
	for name, jobSpec := range jobs {
		// Don't try to parse a reserved section as a job.
		if name == "setup" || name == "teardown" || name == "global" {
			continue
		}

		job := new(Job)
		job.Name = name
		if err := validateJobSection(df, jobSpec, basedir, job); err != nil {
			return fmt.Errorf("Error parsing job %s: %v",
				strconv.Quote(name), err)
		}
		c.Jobs[name] = job
	}

	return nil
}

func parseIniConfig(df DatabaseFlavor, iniConfig *goini.RawConfig, basedir string) (*Config, error) {
	var config = new(Config)

	config.Flavor = df

	if err := decodeGlobalSection(df, iniConfig.GlobalSection, config); err != nil {
		return nil, fmt.Errorf("Error parsing global section: %v", err)
	}
	if err := decodeSetupSection(df, iniConfig.Section("setup"), basedir, &config.Setup); err != nil {
		return nil, fmt.Errorf("Error parsing setup section: %v", err)
	}
	if err := decodeSetupSection(df, iniConfig.Section("teardown"), basedir, &config.Teardown); err != nil {
		return nil, fmt.Errorf("Error parsing teardown section: %v", err)
	}
	if err := decodeConfigJobs(df, iniConfig, basedir, config); err != nil {
		return nil, err
	}

	for name, job := range config.Jobs {
		if config.Duration > 0 && job.Start > config.Duration {
			return nil, fmt.Errorf("job %s starts after test finishes.",
				strconv.Quote(name))
		} else if job.Stop > 0 && config.Duration > 0 && job.Stop > config.Duration {
			return nil, fmt.Errorf("job %s stops after test finishes.",
				strconv.Quote(name))
		}
	}

	return config, nil
}

type JSONConfig struct {
	Duration string                 `json:"duration,omitempty"`
	Errors   []string               `json:"error,omitempty"`
	Setup    ReservedSectionOptions `json:"setup,omitempty"`
	Teardown ReservedSectionOptions `json:"teardown,omitempty"`
	Jobs     map[string]JobOptions  `json:"jobs,omitempty"`
}

func parseJSONConfig(df DatabaseFlavor, jsonConfig JSONConfig, basedir string) (*Config, error) {
	var config = new(Config)

	config.Flavor = df

	if err := validateGlobalSection(jsonConfig, config); err != nil {
		return nil, fmt.Errorf("Error parsing global section: %v", err)
	}
	if err := validateReservedSection(df, jsonConfig, basedir, "Setup", &config.Setup); err != nil {
		return nil, fmt.Errorf("Error parsing setup section: %v", err)
	}
	if err := validateReservedSection(df, jsonConfig, basedir, "Teardown", &config.Teardown); err != nil {
		return nil, fmt.Errorf("Error parsing teardown section: %v", err)
	}
	if err := validateConfigJobs(df, jsonConfig, basedir, config); err != nil {
		return nil, err
	}

	for name, job := range config.Jobs {
		if config.Duration > 0 && job.Start > config.Duration {
			return nil, fmt.Errorf("job %s starts after test finishes.",
				strconv.Quote(name))
		} else if job.Stop > 0 && config.Duration > 0 && job.Stop > config.Duration {
			return nil, fmt.Errorf("job %s stops after test finishes.",
				strconv.Quote(name))
		}
	}

	return config, nil
}

func parseConfig(df DatabaseFlavor, configFile string, baseDir string) (*Config, error) {
	if isJSONFile(configFile) {
		fileContent, err := ioutil.ReadFile(configFile)
		if err != nil {
			return nil, err
		}

		var jsonConfig JSONConfig
		err = json.Unmarshal(fileContent, &jsonConfig)
		if err != nil {
			return nil, err
		}

		return parseJSONConfig(df, jsonConfig, baseDir)
	} else {
		cp := goini.NewRawConfigParser()
		err := cp.ParseFile(configFile)
		if err != nil {
			return nil, err
		}
		iniConfig, err := cp.Finish()
		if err != nil {
			return nil, err
		}
	
		return parseIniConfig(df, iniConfig, baseDir)
	}
}
