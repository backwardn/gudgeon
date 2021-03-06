package web

import (
	rice "github.com/GeertJohan/go.rice"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"html/template"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"strings"

	"github.com/chrisruffalo/gudgeon/version"
)

const (
	templateFileExtension = ".tmpl"
	gzipFileExtension     = ".gz"
)

// cache the content types because we don't really serve that many files
var contentTypeCache = make(map[string]string)

func getContentType(filepath string, defaultType string) string {
	if value, ok := contentTypeCache[filepath]; ok {
		return value
	}

	var contentType string
	if strings.Contains(filepath, ".") {
		ext := filepath[strings.LastIndex(filepath, "."):]
		contentType = mime.TypeByExtension(ext)
	}
	// default if no content type is computed from the extension
	if contentType == "" {
		contentType = defaultType
	}

	// trace logging for mimetype verification, usually commented
	// out unless troubleshooting this code path
	// log.Tracef("%s (mimetype = %s)", filepath, contentType)
	contentTypeCache[filepath] = contentType

	return contentType
}

func (web *web) ServeStatic(fs *rice.Box) gin.HandlerFunc {
	return func(c *gin.Context) {
		url := c.Request.URL

		// dont serve templates
		if strings.HasSuffix(url.Path, templateFileExtension) {
			return
		}

		// for empty path load welcome file (index.html)
		path := url.Path
		if "" == path || "/" == path {
			path = "/index.html"
		}

		// try and open the target file
		file, err := fs.Open(path)

		// if there was an error opening the file or if the file is nil
		// then look for it as a template and if the template is found
		// then process the template
		if err != nil || file == nil {
			// look for .gz (gzipped) file and return that if it exists
			gzipped, err := fs.Open(path + gzipFileExtension)
			if err == nil && gzipped != nil {
				// set gzip output header
				c.Header("Content-Encoding", "gzip")

				// get stat for size
				stat, _ := gzipped.Stat()

				// write output with calculated mime type
				c.DataFromReader(http.StatusOK, stat.Size(), getContentType(path, "application/x-gzip"), gzipped, map[string]string{})

				// close gzipped source
				_ = gzipped.Close()

				// done
				return
			}

			// look for template file and serve it if it exists
			tmpl, err := fs.Open(path + templateFileExtension)
			if err != nil || tmpl == nil {
				// but if it doesn't exist then use the index template
				tmpl, _ = fs.Open("/index.html.tmpl")
			}

			contents, err := ioutil.ReadAll(tmpl)
			if err != nil {
				log.Errorf("Error getting template file contents: %s", err)
			} else {
				parsedTemplate, err := template.New(path).Parse(string(contents))
				if err != nil {
					log.Errorf("Error parsing template file: %s", err)
				}

				// hash
				options := make(map[string]interface{}, 0)
				options["version"] = version.Info()
				options["query_log"] = web.conf.QueryLog.Enabled
				options["query_log_persist"] = web.conf.QueryLog.Persist
				options["metrics"] = web.conf.Metrics.Enabled
				options["metrics_persist"] = web.conf.Metrics.Persist
				options["metrics_detailed"] = web.conf.Metrics.Detailed

				// execute and write template
				c.Status(http.StatusOK)
				err = parsedTemplate.Execute(c.Writer, options)

				// todo: this might not work if the template has already started writing we can't change the status code
				if err != nil {
					c.Status(http.StatusInternalServerError)
					log.Errorf("Error executing template: %s", err)
				}
			}
			return
		}

		// get mime type from extension
		c.Header("Content-Type", getContentType(path, "application/octet-stream"))

		// write file
		c.Status(http.StatusOK)
		_, err = io.Copy(c.Writer, file)
		if err != nil {
			log.Errorf("Writing static output: %s", err)
		}

		// close file
		_ = file.Close()
	}
}
