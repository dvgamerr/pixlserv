package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/PuerkitoBio/throttled"
	"github.com/PuerkitoBio/throttled/store"
	"github.com/go-martini/martini"
	"github.com/martini-contrib/binding"
)

type UploadForm struct {
	PhotoUpload *multipart.FileHeader `form:"photo"`
}

func main() {
	var configFilePath string
	flag.StringVar(&configFilePath, "config", "", "path to a configuration file")
	flag.Parse()

	// Set up logging
	log.SetPrefix("[pixlserv] ")
	log.SetFlags(0) // Remove the timestamp

	// Initialise configuration
	config, err := configInit(configFilePath)
	if err != nil {
		log.Println("Configuration reading failed:", err)
		return
	}
	log.Printf("Running with config: %+v", config)

	// Initialise the cache
	err = cacheInit()
	if err != nil {
		log.Println("Cache initialisation failed:", err)
		return
	}

	// Initialise storage
	err = storageInit(config)
	if err != nil {
		log.Println("Storage initialisation failed:", err)
		return
	}

	// Run the server
	m := martini.Classic()
	if config.throttlingRate > 0 {
		m.Use(throttler(config.throttlingRate))
	}
	m.Get("/image/:parameters/**", func(params martini.Params) (int, string) {
		var parameters Params
		transformationName := parseTransformationName(params["parameters"])
		if transformationName != "" {
			var ok bool
			parameters, ok = config.transformations[transformationName]
			if !ok {
				return http.StatusBadRequest, "Unknown transformation: " + transformationName
			}
		} else if config.allowCustomTransformations {
			parameters, err = parseParameters(params["parameters"])
		} else {
			return http.StatusBadRequest, "Custom transformations not allowed"
		}
		baseImagePath, scale := parseBasePathAndScale(params["_1"])
		if config.allowCustomScale {
			parameters = parameters.WithScale(scale)
		}

		if err != nil {
			return http.StatusBadRequest, err.Error()
		}
		log.Println("Parameters:", parameters)

		// Check if the image with the given parameters already exists
		// and return it
		fullImagePath, _ := createFilePath(baseImagePath, parameters)
		img, format, err := loadFromCache(fullImagePath)
		if err == nil {
			var buffer bytes.Buffer
			writeImage(img, format, &buffer)

			return http.StatusOK, buffer.String()
		}

		// Load the original image and process it
		if !imageExists(baseImagePath) {
			return http.StatusNotFound, "Image not found: " + baseImagePath
		}

		img, format, err = loadImage(baseImagePath)
		if err != nil {
			return http.StatusInternalServerError, err.Error()
		}

		imgNew := transformCropAndResize(img, parameters)

		var buffer bytes.Buffer
		err = writeImage(imgNew, format, &buffer)
		if err != nil {
			log.Println("Writing an image to the response failed:", err)
		}

		// Cache the image asynchronously to speed up the response
		go func() {
			err = addToCache(fullImagePath, imgNew, format)
			if err != nil {
				log.Println("Saving an image to cache failed:", err)
			}
		}()

		return http.StatusOK, buffer.String()
	})
	m.Post("/upload", binding.MultipartForm(UploadForm{}), func(uf UploadForm) (int, string) {
		file, err := uf.PhotoUpload.Open()
		if err != nil {
			return http.StatusBadRequest, err.Error()
		}

		img, format, err := image.Decode(file)
		if err != nil {
			return http.StatusBadRequest, err.Error()
		}

		defer file.Close()

		// Not a big fan of .jpeg file extensions
		fileName := fmt.Sprintf("%d.%s", time.Now().Unix(), strings.Replace(format, "jpeg", "jpg", 1))

		if config.asyncUploads {
			go func() {
				saveImage(img, format, fileName)
			}()
		} else {
			err = saveImage(img, format, fileName)
			if err != nil {
				return http.StatusInternalServerError, err.Error()
			}
		}
		return http.StatusOK, ""
	})
	go m.Run()

	// Wait for when the program is terminated
	ch := make(chan os.Signal)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch

	// Clean up
	cacheCleanUp()
	storageCleanUp()
}

func throttler(perMinRate int) http.Handler {
	t := throttled.RateLimit(throttled.PerMin(perMinRate), &throttled.VaryBy{RemoteAddr: true}, store.NewMemStore(1000))
	return t.Throttle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Nothing needed here
	}))
}
