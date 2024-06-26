package service

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"time"

	"github.com/spf13/viper"
	"github.com/veron-baranige/springboot-app-monitor/internal/monitor"
	"gopkg.in/gomail.v2"
)

type MonitorConfig struct {
	AppLogoPath           string
	TestConnectivityUrl   string
	AlertSoundPath        string

	UrlsToMonitor         []string
	MonitorInterval       time.Duration
	CpuUsageWarnThreshold uint32
	JvmUsageWarnThreshold uint32

	MailDialer       *gomail.Dialer
	EmailReceipients []string

	IsDesktopAlertsEnabled bool
	IsEmailAlertsEnabled   bool
}

type MonitorService struct {
	config MonitorConfig
}

func NewMonitorService(config MonitorConfig) *MonitorService {
	return &MonitorService{
		config: config,
	}
}

func (ms *MonitorService) Start() {
	log.Println("started monitoring service")

	ms.monitorHealthAndMetrics()

	ticker := time.NewTicker(ms.config.MonitorInterval)
	defer ticker.Stop()

	for range ticker.C {
		ms.monitorHealthAndMetrics()
	}
}

func (ms *MonitorService) monitorHealthAndMetrics() {
	if !hasInternetConnection(ms.config.TestConnectivityUrl) {
		log.Println("No internect connection available for monitoring. Skipping monitoring for now.")
		// msg := fmt.Sprintf("[%s] NO INTERNET CONNECTIVITY", time.Now().Format("15:04"))
		// notify.Notify("Spring Boot App Monitor", "Spring Boot App Monitor", msg, ms.config.AppLogoPath)
		return
	}

	for i, baseUrl := range ms.config.UrlsToMonitor {
		go func(baseUrl string) {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
			defer cancel()

			status, err := monitor.GetHealthStatus(ctx, baseUrl)
			if err != nil {
				if errors.Is(err, monitor.ErrNoActuatorSupport) {
					log.Printf("No actuator support for: %s", baseUrl)
					msg := fmt.Sprintf("[%s] No actuator support for: %s", 
						time.Now().Format("15:04"), baseUrl+"/actuator")
					ms.handleAlert(baseUrl, msg, true, true)
					return
				}

				if errors.Is(err, monitor.ErrNotResponding) {
					log.Printf("Timeout exceeded. No response from: %s", baseUrl)
					msg := fmt.Sprintf("[%s] %s", 
						time.Now().Format("15:04"), "No response from app. Attention required!")
					ms.handleAlert(baseUrl, msg, true, true)
					return
				}

				log.Printf("Failed to get health status. Err: %s", err)
				msg := fmt.Sprintf("[%s] Failed to get health status: %s", time.Now().Format("15:04"), err)
				ms.handleAlert(baseUrl, msg, true, true)
				return
			}

			if status != monitor.Up {
				log.Printf("Health status: %s for: %s", status, baseUrl)
				msg := fmt.Sprintf("[%s] Health Status: %s. Attention required!", 
					time.Now().Format("15:04"), string(status))
				ms.handleAlert(baseUrl, msg, true, true)
				return
			}

			metrics, err := monitor.GetMetrics(ctx, baseUrl)
			if err != nil {
				log.Printf("Failed to get metrics for: %s. Err: %s", baseUrl, err)
				msg := fmt.Sprintf("[%s] Health status: %s. Failed to get metrics: %s", 
					time.Now().Format("15:04"), string(status), err)
				ms.handleAlert(baseUrl, msg, false, false)
				return
			}

			exceededCpuUsageThreshold := metrics.CpuUsage * metrics.CpuCount > float64(ms.config.CpuUsageWarnThreshold)
			exceededJvmUsageThreshold := metrics.MemoryTotal > 0.0 && 
				(metrics.MemoryUsed / metrics.MemoryTotal)*100 > float64(ms.config.JvmUsageWarnThreshold)

			if exceededCpuUsageThreshold || exceededJvmUsageThreshold {
				log.Printf("Exceeded CPU/JVM threshold: %s", baseUrl)
                msg := fmt.Sprintf("[%s] Attention required! CPU: %.2f%%, JVM: %.1f/%.1f GB", 
					time.Now().Format("15:04"), metrics.CpuUsage*metrics.CpuCount, metrics.MemoryUsed, metrics.MemoryTotal)
                ms.handleAlert(baseUrl, msg, true, true)
                return
            }
			    
			log.Printf("[%v] CPU: %.2f%%, JVM: %.1f/%.1f GB", 
				baseUrl, metrics.CpuUsage*metrics.CpuCount, metrics.MemoryUsed, metrics.MemoryTotal)
			msg := fmt.Sprintf("[%v] CPU: %.2f%%, JVM: %.1f/%.1f GB",
				time.Now().Format("15:04"), metrics.CpuUsage*metrics.CpuCount, metrics.MemoryUsed, metrics.MemoryTotal)
			ms.handleAlert(baseUrl, msg, false, false)
		}(baseUrl)

		// wait before monitoring next app to provide notification read time
		if len(ms.config.UrlsToMonitor) > 1 && i == len(ms.config.UrlsToMonitor)-1 {
			time.Sleep(6 * time.Second)
		}
	}
}

func (ms *MonitorService) handleAlert(appBaseUrl string, msgContent string, isAlert bool, sendMail bool) {
	if ms.config.IsDesktopAlertsEnabled {
		if !isAlert {
			if output, err := exec.Command("notify-send", "-u", "normal", "-i", ms.config.AppLogoPath, appBaseUrl, msgContent).CombinedOutput(); err != nil {
				log.Printf("failed to send desktop notification: err: %v, output: %v", err, string(output))
			}
		} else {
			if output, err := exec.Command("notify-send", "-u", "critical", "-i", ms.config.AppLogoPath, appBaseUrl, msgContent).CombinedOutput(); err != nil {
				log.Printf("failed to send desktop alert: err: %v, output: %v", err, string(output))
			} else if output, err := exec.Command("gst-play-1.0", ms.config.AlertSoundPath).CombinedOutput(); err != nil {
				log.Printf("error playing alert sound: %s %s", err, string(output))
			}
		}
	}

	if ms.config.IsEmailAlertsEnabled && sendMail {
		mailErr := sendEmail(ms.config.MailDialer, ms.config.EmailReceipients, "Spring Boot App Monitor - " + appBaseUrl, msgContent)
		if mailErr != nil {
			log.Println("Failed to send email: ", mailErr)
		}
	}
}

func sendEmail(mailDialer *gomail.Dialer, recipients []string, subject string, body string) error {
	message := gomail.NewMessage()

	message.SetHeader("From", viper.GetString("SMTP_USER"))
	message.SetHeader("To", recipients...)
	message.SetHeader("Subject", subject)
	message.SetBody("text/plain", body)

	mailDialer.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	return mailDialer.DialAndSend(message)
}	

func hasInternetConnection(testConnUrl string) bool {
	_, err := http.Get(testConnUrl)
	return err == nil
}