package main

import (
	"fmt"
	"log"
	"maploader/config"
	"maploader/robot"
	"maploader/tar"
	"maploader/util"
	"path/filepath"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

var stateTopic string      // mqtt-prefix/mqtt-identifier/maploader/status  (e.g., /valetudo/myrobot/maploader/status)
var currentMapTopic string // mqtt-prefix/mqtt-identifier/maploader/map  (e.g., /valetudo/myrobot/maploader/map)
var saveTopic string       // currentMapTopic + "/save"
var loadTopic string       // currentMapTopic + "/load"
var setTopic string        // currentMapTopic + "/set"

var maploaderDir string
var currentMap string

var rotationKeepMaps int

var messageStateTopicHandler mqtt.MessageHandler = func(client mqtt.Client, msg mqtt.Message) {
	log.Printf("Received message: %s from topic: %s\n", msg.Payload(), msg.Topic())
	if string(msg.Payload()) != currentMap {
		log.Printf("Loaded current map from status topic")
		currentMap = string(msg.Payload())
	}
}

var messageSaveTopicHandler mqtt.MessageHandler = func(client mqtt.Client, msg mqtt.Message) {
	log.Printf("Received message: %s from topic: %s\n", msg.Payload(), msg.Topic())
	saveMap(client, string(msg.Payload()))
}

var messageLoadTopicHandler mqtt.MessageHandler = func(client mqtt.Client, msg mqtt.Message) {
	log.Printf("Received message: %s from topic: %s\n", msg.Payload(), msg.Topic())
	loadMap(client, string(msg.Payload()))
}

var messageSetTopicHandler mqtt.MessageHandler = func(client mqtt.Client, msg mqtt.Message) {
	log.Printf("Received message: %s from topic: %s\n", msg.Payload(), msg.Topic())
	saveMap(client, currentMap)
	loadMap(client, string(msg.Payload()))
}

var connectHandler mqtt.OnConnectHandler = func(client mqtt.Client) {
	log.Println("Connected")

	subscriptions := []struct {
		Topic   string
		Handler mqtt.MessageHandler
	}{
		{currentMapTopic, messageStateTopicHandler},
		{saveTopic, messageSaveTopicHandler},
		{loadTopic, messageLoadTopicHandler},
		{setTopic, messageSetTopicHandler},
	}
	for _, sub := range subscriptions {
		token := client.Subscribe(sub.Topic, 1, sub.Handler)
		token.Wait()
		log.Printf("Subscribed to topic: %s", sub.Topic)
	}
}

var connectLostHandler mqtt.ConnectionLostHandler = func(client mqtt.Client, err error) {
	log.Printf("Connect lost: %v", err)
}

func main() {
	currentMap = config.Getenv("DEFAULT_MAP_NAME", "main")
	maploaderDir = config.Getenv("MAPLOADER_DIR", "/data/maploader")
	rotationKeepMaps = config.RotationKeepMaps()

	util.InitLogging(maploaderDir + "/log")
	config.InitConfig(config.Getenv("VALETUDO_CONFIG_PATH", "/data/valetudo_config.json"))

	robot.DetectRobot()

	initTopics()

	var broker = config.MqttHost()
	var port = config.MqttPort()
	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("tcp://%s:%d", broker, port))
	opts.SetClientID("maploader_" + config.MqttIdentifier())
	opts.SetUsername(config.MqttUsername())
	opts.SetPassword(config.MqttPassword())
	opts.OnConnect = connectHandler
	opts.OnConnectionLost = connectLostHandler

	opts.WillTopic = stateTopic
	opts.WillEnabled = true
	opts.WillPayload = []byte("offline")
	opts.WillRetained = true

	client := mqtt.NewClient(opts)
	retry := time.NewTicker(5 * time.Second)
	for range retry.C {
		if token := client.Connect(); token.Wait() && token.Error() != nil {
			//handle error
		} else {
			retry.Stop()
			break
		}
	}
	publishState(client, "idle")
	for {
		time.Sleep(time.Second)
	}
}

func publishCurrentMap(client mqtt.Client) {
	token := client.Publish(currentMapTopic, 0, true, currentMap)
	token.Wait()
	time.Sleep(time.Second)
}

func publishState(client mqtt.Client, status string) {
	token := client.Publish(stateTopic, 0, true, status)
	token.Wait()
	time.Sleep(time.Second)
}

// Save the current map as the given name
func saveMap(client mqtt.Client, mapName string) {
	if mapName == "" {
		mapName = currentMap
	}

	publishState(client, "saving_map")
	log.Printf("Saving current map as %s\n", mapName)

	err := util.RotateFile(rotationKeepMaps, filepath.Join(maploaderDir, mapName), "tar.gz")
	checkAndHandleErrorWithMqtt(err, client)

	err = tar.Tar(fmt.Sprintf("%s/%s.tar.gz", maploaderDir, mapName), robot.CurrentRobot.MapFilesAndFolders()...)
	checkAndHandleErrorWithMqtt(err, client)

	currentMap = mapName
	publishCurrentMap(client)
	publishState(client, "idle")
}

// Load the map of the given name
func loadMap(client mqtt.Client, mapName string) {
	if mapName == "" {
		mapName = currentMap
	}

	mapFileToLoadMatches, err := filepath.Glob(filepath.Join(maploaderDir, mapName+".tar.gz"))
	checkAndHandleErrorWithMqtt(err, client)

	publishState(client, "loading_map")
	log.Printf("Attempting to load map %s\n", mapName)
	log.Println("stopping processes")
	robot.StopProcesses()

	log.Println("removing current map files")
	util.RemoveDirContents(robot.CurrentRobot.MapFolders...)

	if len(mapFileToLoadMatches) == 0 {
		log.Println("requested map does not exist, loading blank map")
	} else {
		log.Println("loading map from archive")
		err = tar.Untar(mapFileToLoadMatches[0], "/")
		checkAndHandleErrorWithMqtt(err, client)
	}

	log.Println("map load complete, syncing files")
	robot.ExcuteCmd("sync")

	log.Println("restarting processes")
	robot.StartProcesses()

	currentMap = mapName
	publishCurrentMap(client)
	publishState(client, "idle")
}

func checkAndHandleErrorWithMqtt(err error, client mqtt.Client) {
	if err != nil {
		util.CheckAndHandleError(err)
		publishState(client, "error")
	}
}

func initTopics() {
	var prefix string

	prefix = config.MqttPrefix()

	if prefix == "" {
		prefix = "valetudo"
	}
	prefix = prefix + "/" + config.MqttIdentifier() + "/maploader"

	stateTopic = prefix + "/status"
	currentMapTopic = prefix + "/map"
	saveTopic = currentMapTopic + "/save"
	loadTopic = currentMapTopic + "/load"
	setTopic = currentMapTopic + "/set"

	log.Printf("stateTopic: %s", stateTopic)
	log.Printf("currentMapTopic: %s", currentMapTopic)
	log.Printf("saveTopic: %s", saveTopic)
	log.Printf("loadTopic: %s", loadTopic)
	log.Printf("setTopic: %s", setTopic)
}
