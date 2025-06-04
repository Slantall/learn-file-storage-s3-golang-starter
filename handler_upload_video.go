package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	const maxMemory = 10 << 30 // 1 GB
	r.ParseMultipartForm(maxMemory)

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't find video", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not authorized to update this video", nil)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", nil)
		return
	}
	tempPath := "temp-tubely-upload.mp4"
	tmpFile, err := os.CreateTemp("", tempPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not create temporary file", nil)
		return
	}

	defer os.Remove(tempPath)
	defer tmpFile.Close()

	_, err = io.Copy(tmpFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error saving temporary file", err)
		return
	}

	_, err = tmpFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error returning to the start of the temporary file", err)
		return
	}
	assetPath := getAssetPath(mediaType)

	ratio, err := getVideoAspectRatio(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error determining the ratio of the temporary file", err)
		return
	}
	fullPath := fmt.Sprintf("%s/%s", ratio, assetPath)

	putInput := &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fullPath,
		Body:        tmpFile,
		ContentType: &mediaType,
	}
	_, err = cfg.s3Client.PutObject(context.Background(), putInput)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error returning to the start of the temporary file", err)
		return
	}

	url := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, fullPath)
	fmt.Println(url)

	video.VideoURL = &url
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

}

func getVideoAspectRatio(filePath string) (string, error) {
	probe := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var probeOut bytes.Buffer
	probe.Stdout = &probeOut
	probe.Run()

	type Streams struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}

	type ProbeJSON struct {
		Streams []Streams `json:"streams"`
	}

	var probeJSON ProbeJSON
	if err := json.Unmarshal(probeOut.Bytes(), &probeJSON); err != nil {
		return "", err
	}
	if len(probeJSON.Streams) == 0 {
		return "", fmt.Errorf("ffprobe returned an empty stream")
	}
	ratio := float64(probeJSON.Streams[0].Width) / float64(probeJSON.Streams[0].Height)
	if ratio < 1.79 && ratio > 1.76 {
		return "landscape", nil
	}
	if ratio < 0.57 && ratio > 0.56 {
		return "portrait", nil
	}
	return "other", nil
}
