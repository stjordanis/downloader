package main

// Config holds the app's configuration
type Config struct {
	Redis struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"redis"`

	Processor struct {
		StorageDir string `json:"storage_dir"`
	} `json:"processor"`

	Notifier struct {
		Concurrency int `json:"concurrency"`
	} `json:"notifier"`
}
