/*
 * Copyright (C) 2016 Red Hat, Inc.
 *
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 *
 */

package orientdb

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/skydive-project/skydive/common"
	"github.com/skydive-project/skydive/filters"
)

type Document map[string]interface{}

type Result struct {
	Result []Document `json:"result"`
}

type Client struct {
	url           string
	authenticated bool
	database      string
	username      string
	password      string
	cookies       []*http.Cookie
	client        *http.Client
}

type Session struct {
	client   *Client
	database string
}

type Error struct {
	Code    int    `json:"code"`
	Reason  int    `json:"reason"`
	Content string `json:"content"`
}

type Errors struct {
	Errors []Error `json:"errors"`
}

type Property struct {
	Name        string `json:"name,omitempty"`
	Type        string `json:"type,omitempty"`
	LinkedType  string `json:"linkedType,omitempty"`
	LinkedClass string `json:"linkedClass,omitempty"`
	Mandatory   bool   `json:"mandatory"`
	NotNull     bool   `json:"notNull"`
	ReadOnly    bool   `json:"readonly"`
	Collate     string `json:"collate,omitempty"`
	Regexp      string `json:"regexp,omitempty"`
}

type Index struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	Fields []string `json:"fields"`
}

type ClassDefinition struct {
	Name         string     `json:"name"`
	SuperClass   string     `json:"superClass,omitempty"`
	SuperClasses []string   `json:"superClasses,omitempty"`
	Abstract     bool       `json:"abstract"`
	StrictMode   bool       `json:"strictmode"`
	Alias        string     `json:"alias,omitempty"`
	Properties   []Property `json:"properties,omitempty"`
	Indexes      []Index    `json:"indexes,omitempty"`
}

type DocumentClass struct {
	Class ClassDefinition `json:"class"`
}

func parseError(body io.Reader) error {
	var errs Errors
	if err := common.JsonDecode(body, &errs); err != nil {
		return fmt.Errorf("Error while parsing error: %s (%s)", err.Error(), body)
	}
	var s string
	for _, err := range errs.Errors {
		s += err.Content + "\n"
	}
	return errors.New(s)
}

func getResponseBody(resp *http.Response) (io.ReadCloser, error) {
	if encoding := resp.Header.Get("Content-Encoding"); encoding == "gzip" {
		decompressor, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		return decompressor, nil
	} else {
		return resp.Body, nil
	}
}

func parseResponse(resp *http.Response, result interface{}) error {
	if resp.StatusCode < 400 && resp.ContentLength == 0 {
		return nil
	}

	body, err := getResponseBody(resp)
	if err != nil {
		return err
	}
	defer body.Close()

	if resp.StatusCode >= 400 {
		return parseError(body)
	} else {
		content, _ := ioutil.ReadAll(body)
		if len(content) != 0 {
			if err := common.JsonDecode(bytes.NewBuffer(content), result); err != nil {
				return fmt.Errorf("Error while parsing OrientDB response: %s (%s)", err.Error(), content)
			}
		}
	}

	return nil
}

func compressBody(body io.Reader) io.Reader {
	buffer := new(bytes.Buffer)
	compressor := gzip.NewWriter(buffer)
	io.Copy(compressor, body)
	compressor.Close()
	return buffer
}

func replaceSlashes(key string) string {
	return strings.Replace(key, "/", ".", -1)
}

func FilterToExpression(f *filters.Filter, prefix string) string {
	if f.BoolFilter != nil {
		keyword := ""
		switch f.BoolFilter.Op {
		case filters.BoolFilterOp_NOT:
			return "NOT " + FilterToExpression(f.BoolFilter.Filters[0], prefix)
		case filters.BoolFilterOp_OR:
			keyword = "OR"
		case filters.BoolFilterOp_AND:
			keyword = "AND"
		}
		var conditions []string
		for _, item := range f.BoolFilter.Filters {
			if expr := FilterToExpression(item, prefix); expr != "" {
				conditions = append(conditions, "("+FilterToExpression(item, prefix)+")")
			}
		}
		return strings.Join(conditions, " "+keyword+" ")
	}

	if f.TermStringFilter != nil {
		return fmt.Sprintf(`%s = "%s"`, prefix+replaceSlashes(f.TermStringFilter.Key), f.TermStringFilter.Value)
	}

	if f.TermInt64Filter != nil {
		return fmt.Sprintf(`%s = %d`, prefix+replaceSlashes(f.TermInt64Filter.Key), f.TermInt64Filter.Value)
	}

	if f.GtInt64Filter != nil {
		return fmt.Sprintf("%v > %v", prefix+replaceSlashes(f.GtInt64Filter.Key), f.GtInt64Filter.Value)
	}

	if f.LtInt64Filter != nil {
		return fmt.Sprintf("%v < %v", prefix+replaceSlashes(f.LtInt64Filter.Key), f.LtInt64Filter.Value)
	}

	if f.GteInt64Filter != nil {
		return fmt.Sprintf("%v >= %v", prefix+replaceSlashes(f.GteInt64Filter.Key), f.GteInt64Filter.Value)
	}

	if f.LteInt64Filter != nil {
		return fmt.Sprintf("%v <= %v", prefix+replaceSlashes(f.LteInt64Filter.Key), f.LteInt64Filter.Value)
	}

	if f.RegexFilter != nil {
		return fmt.Sprintf(`%s MATCHES "%s"`, prefix+replaceSlashes(f.RegexFilter.Key), f.RegexFilter.Value)
	}

	return ""
}

func NewClient(url string, database string, username string, password string) (*Client, error) {
	client := &Client{
		url:      url,
		database: database,
		username: username,
		password: password,
		client:   &http.Client{},
	}

	_, err := client.GetDatabase()
	if err != nil {
		if _, err := client.CreateDatabase(); err != nil {
			return nil, err
		}
	}

	if err := client.Connect(); err != nil {
		return nil, err
	}

	return client, nil
}

func (c *Client) Request(method string, url string, body io.Reader) (*http.Response, error) {
	if body != nil {
		body = compressBody(body)
	}

	request, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}

	if !c.authenticated {
		request.SetBasicAuth(c.username, c.password)
	} else {
		for _, cookie := range c.cookies {
			request.AddCookie(cookie)
		}
	}

	resp, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == 401 {
		if err := c.Connect(); err != nil {
			return nil, err
		}
		resp, err = c.client.Do(request)
	}

	return resp, err
}

func (c *Client) DeleteDocument(id string) error {
	url := fmt.Sprintf("%s/document/%s/%s", c.url, c.database, id)
	resp, err := c.Request("DELETE", url, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return parseError(resp.Body)
	}
	return nil
}

func (c *Client) GetDocument(id string) (Document, error) {
	url := fmt.Sprintf("%s/document/%s/%s", c.url, c.database, id)
	resp, err := c.Request("GET", url, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result Document
	if err := parseResponse(resp, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) CreateDocument(doc Document) (Document, error) {
	url := fmt.Sprintf("%s/document/%s", c.url, c.database)
	marshal, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}

	resp, err := c.Request("POST", url, bytes.NewBuffer(marshal))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result Document
	if err := parseResponse(resp, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) Upsert(doc Document, key string) (Document, error) {
	class, ok := doc["@class"]
	if !ok {
		return nil, errors.New("A @class property is required for upsert")
	}
	delete(doc, "@class")

	id, ok := doc[key]
	if !ok {
		return nil, fmt.Errorf("No property '%s' found in document", key)
	}

	content, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}

	query := fmt.Sprintf("UPDATE %s CONTENT %s UPSERT RETURN AFTER @rid WHERE %s = '%s'", class, string(content), key, id)
	docs, err := c.Sql(query)

	if len(docs) > 0 {
		return docs[0], err
	}

	return nil, err
}

func (c *Client) GetDocumentClass(name string) (*DocumentClass, error) {
	url := fmt.Sprintf("%s/class/%s/%s", c.url, c.database, name)
	resp, err := c.Request("GET", url, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result DocumentClass
	if err := parseResponse(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) AlterProperty(className string, prop Property) error {
	alterQuery := fmt.Sprintf("ALTER PROPERTY %s.%s", className, prop.Name)
	if prop.Mandatory {
		if _, err := c.Sql(alterQuery + " MANDATORY true"); err != nil && err != io.EOF {
			return err
		}
	}
	if prop.NotNull {
		if _, err := c.Sql(alterQuery + " NOTNULL true"); err != nil && err != io.EOF {
			return err
		}
	}
	if prop.ReadOnly {
		if _, err := c.Sql(alterQuery + " READONLY true"); err != nil && err != io.EOF {
			return err
		}
	}
	return nil
}

func (c *Client) CreateProperty(className string, prop Property) error {
	query := fmt.Sprintf("CREATE PROPERTY %s.%s %s", className, prop.Name, prop.Type)
	if prop.LinkedClass != "" {
		query += " " + prop.LinkedClass
	}
	if prop.LinkedType != "" {
		query += " " + prop.LinkedType
	}
	if _, err := c.Sql(query); err != nil {
		return err
	}

	return c.AlterProperty(className, prop)
}

func (c *Client) CreateClass(class ClassDefinition) error {
	query := fmt.Sprintf("CREATE CLASS %s", class.Name)
	if class.SuperClass != "" {
		query += " EXTENDS " + class.SuperClass
	}

	_, err := c.Sql(query)
	return err
}

func (c *Client) CreateIndex(className string, index Index) error {
	query := fmt.Sprintf("CREATE INDEX %s ON %s (%s) %s", index.Name, className, strings.Join(index.Fields, ", "), index.Type)
	_, err := c.Sql(query)
	return err
}

func (c *Client) CreateDocumentClass(class ClassDefinition) error {
	if err := c.CreateClass(class); err != nil {
		return err
	}

	for _, prop := range class.Properties {
		if err := c.CreateProperty(class.Name, prop); err != nil {
			return err
		}
	}

	for _, index := range class.Indexes {
		if err := c.CreateIndex(class.Name, index); err != nil {
			return err
		}
	}

	return nil
}

func (c *Client) DeleteDocumentClass(name string) error {
	url := fmt.Sprintf("%s/class/%s/%s", c.url, c.database, name)
	resp, err := c.Request("DELETE", url, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return parseError(resp.Body)
	}
	return nil
}

func (c *Client) GetDatabase() (Document, error) {
	url := fmt.Sprintf("%s/database/%s", c.url, c.database)
	resp, err := c.Request("GET", url, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result Document
	if err := parseResponse(resp, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) CreateDatabase() (Document, error) {
	url := fmt.Sprintf("%s/database/%s/plocal", c.url, c.database)
	resp, err := c.Request("POST", url, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result Document
	// OrientDB returns a 500 error but successfully creates the DB
	parseResponse(resp, &result)

	if _, e := c.GetDatabase(); e != nil {
		// Returns the original error
		return nil, err
	}

	return result, nil
}

func (c *Client) Sql(query string) ([]Document, error) {
	url := fmt.Sprintf("%s/command/%s/sql", c.url, c.database)
	resp, err := c.Request("POST", url, bytes.NewBufferString(query))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result Result
	if err := parseResponse(resp, &result); err != nil {
		return nil, err
	}
	return result.Result, nil
}

func (c *Client) Query(obj string, query *filters.SearchQuery) ([]Document, error) {
	interval := query.PaginationRange
	filter := query.Filter

	sql := "SELECT FROM " + obj
	if conditional := FilterToExpression(filter, ""); conditional != "" {
		sql += " WHERE " + conditional
	}

	if interval != nil {
		sql += fmt.Sprintf(" LIMIT %d, %d", interval.To-interval.From, interval.From)
	}

	if query.Sort {
		sql += " ORDER BY " + query.SortBy
	}

	return c.Sql(sql)
}

func (c *Client) Connect() error {
	url := fmt.Sprintf("%s/connect/%s", c.url, c.database)
	request, err := http.NewRequest("GET", url, nil)
	request.SetBasicAuth(c.username, c.password)

	resp, err := c.client.Do(request)
	if err != nil {
		return err
	}

	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("Failed to authenticate to OrientDB: %s", resp.Status)
	}

	if resp.StatusCode < 400 && len(resp.Cookies()) != 0 {
		c.authenticated = true
		c.cookies = resp.Cookies()
	}

	return nil
}
