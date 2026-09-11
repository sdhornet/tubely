package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const uploadLimit = 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, uploadLimit)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse video ID", err)
		return
	}

	bearerToken, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", err)
		return
	}

	userID, err := auth.ValidateJWT(bearerToken, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to retrieve video", err)
		return
	}
	if userID != video.UserID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", nil)
		return
	}

	fmt.Println("uploading video", video.ID, "by user", userID)

	videoFile, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse video file", err)
		return
	}
	defer videoFile.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid Media Type", nil)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to save video", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	if _, err = io.Copy(tempFile, videoFile); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to save video", err)
		return
	}

	if _, err = tempFile.Seek(0, io.SeekStart); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to seek video", err)
		return
	}

	fastVidPath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Video processing failed", err)
		return
	}
	fastVid, err := os.Open(fastVidPath)
	defer os.Remove(fastVid.Name())
	defer fastVid.Close()

	aspectRatio, err := getVideoAspectRation(fastVid.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error Parsing video metadata", err)
		return
	}

	randData := make([]byte, 16)
	rand.Read(randData)
	keyBase := hex.EncodeToString(randData)
	var ratioPrefix string

	switch aspectRatio {
	case "16:9":
		ratioPrefix = "landscape"
	case "9:16":
		ratioPrefix = "portrait"
	default:
		ratioPrefix = "other"
	}

	key := ratioPrefix + "/" + keyBase + ".mp4"

	if _, err = cfg.s3Client.PutObject(r.Context(),
		&s3.PutObjectInput{
			Bucket:      aws.String(cfg.s3Bucket),
			Key:         aws.String(key),
			Body:        fastVid,
			ContentType: aws.String(mediaType)}); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to save the video", err)
		return
	}

	vidURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, key)

	video.VideoURL = &vidURL
	if err := cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to update video URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRation(filePath string) (string, error) {
	type FFProbeOutput struct {
		Streams []struct {
			Width  float64 `json:"width,omitempty"`
			Height float64 `json:"height,omitempty"`
		} `json:"streams"`
	}

	vidStats := FFProbeOutput{}
	output := &bytes.Buffer{}

	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		return "", errors.New("Failed to run video parsing command")
	}

	if err := json.Unmarshal(output.Bytes(), &vidStats); err != nil {
		return "", errors.New("Failed to extract video metadata")
	}

	var ratio float64 = vidStats.Streams[0].Width / vidStats.Streams[0].Height

	switch math.Round(ratio*100) / 100 {
	case 1.78:
		return "16:9", nil
	case 0.56:
		return "9:16", nil
	default:
		return "other", nil
	}
}

func processVideoForFastStart(filePath string) (string, error) {
	fastVid := filePath + ".processing"

	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", fastVid)
	if err := cmd.Run(); err != nil {
		return "", errors.New("Failed to process video for streaming")
	}

	return fastVid, nil
}
