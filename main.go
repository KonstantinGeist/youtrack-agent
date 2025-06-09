package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	youtrackURL         string
	youtrackToken       string
	llmURL              string
	llmAPIKey           string
	modelName           string
	maxSteps            int
	youtrackSearchLimit int
)

var expertSystemPrompt = `Ты эксперт по поиску в YouTrack. Твои действия:
  1. Читай вопрос пользователя и список найденных задач.
  2. Если информации достаточно - реши, можно ли выдать финальный ответ на основе полученной информации.
  3. Если информации пока мало для полноценного ответа - сформулируй новый YouTrack-запрос (с учётом синтаксиса запросов Youtrack), который, вероятно, позволит, лучше найти нужные данные.
  4. Учти, что если ты ищешь текст в свободной форме (т.е. НЕ свойства или ключевые слова YouTrack-запроса), то нужно использовать русский язык для такого текста.
  5. Твой ответ читает твой ассистент, поэтому тебе нужно однозначно сообщить, достаточно ли полученной информации, или нужно продолжать поиск.
  6. Рассуждай по-русски.
  7. Допустимые статусы (state) у задачи (issue) в Youtrack: Open, Submitted, In Progress, Code Review, Fixed, To Be Merged, Verified.
  8. Допустимые приоритеты (priority) задач: Minor, Normal, Critical, Blocker.
  9. НИКОГДА не ищи по типу задачи (type).
  10. Ищи по статусу ТОЛЬКО ЕСЛИ это ЯВНО попросил пользователь. Ищи по приоритету ТОЛЬКО ЕСЛИ это ЯВНО попросил пользователь.
  11. В Youtrack-запросах используй знак минуса для отрицания, например state: -YourStatus
  12. Используй в Youtrack-запросах двоеточие для сравнения, например description: "твое описание"
  13. НЕ пытайся использовать сортировку в Youtrack-запросах.

Примеры запросов:
- "найти задачи в проекте CRM в статусе Blocker у пользователя Константин Гейст" -- project: CRM state: Blocker assignee: konstantin.geyst
- "найти задачи в проекте ISONLINE, где упоминается слово оргструктура" -- project: ISONLINE AND (description: "оргструктура" OR summary: "оргструктура")
- "найти задачи в проекте ISOF в статусе Open, но не в статусе Submitted, где упоминается React" -- project: ISOF AND state: Open AND state: -Submitted AND (description: "React" OR summary: "React")
и т.д.

Сначала думай, только потом придумай Youtrack-запрос.
`

var assistantSystemPrompt = fmt.Sprintf(`/no_think Ты ассистент эксперта по поиску в Youtrack. Твои действия:
  1. Прочитай то, что тебе передал эксперт. Если эксперт однозначно ответил на вопрос пользователя, то action=final_answer. Иначе action=yt_search. 
  2. Трансформируй результат его размышлений в заданную ниже схему. Строго следуй мышлению эксперта и не отклоняйся от него.
  3. Если эксперт сообщает, что однозначный ответ найден, то в финальном ответе не упоминай Youtrack-запросы, а сообщи только ответ (не забудь action=final_answer в таком случае)
  4. Если эксперт сообщает, что нужно ещё поискать, тогда action=yt_search и в параметр query передай сгенерированный им (экспертом) Youtrack-запрос.
  5. В answer отвечай на русском (если его нужно заполнить).
  6. Когда упоминаешь задачу, ВСЕГДА выдавай полную ссылку (формата \"%s/issue/{issueID}\").
  7. В поле answer всегда используй Markdown.
`, youtrackURL)

type StreamWriter interface {
	Write([]byte) (int, error)
	Flush()
	CloseNotify() <-chan bool
}

type wrappedWriter struct {
	w http.ResponseWriter
	f http.Flusher
	c http.CloseNotifier
}

func (ww wrappedWriter) Write(p []byte) (int, error) { return ww.w.Write(p) }
func (ww wrappedWriter) Flush()                      { ww.f.Flush() }
func (ww wrappedWriter) CloseNotify() <-chan bool    { return ww.c.CloseNotify() }

func buildChunk(id, content string, roleIncluded, finish bool) []byte {
	now := time.Now().Unix()
	chunk := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": now,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{},
			},
		},
	}
	delta := chunk["choices"].([]map[string]interface{})[0]["delta"].(map[string]interface{})
	if roleIncluded {
		delta["role"] = "assistant"
	}
	if content != "" {
		delta["content"] = content
	}
	if finish {
		chunk["choices"].([]map[string]interface{})[0]["finish_reason"] = "stop"
	}
	b, _ := json.Marshal(chunk)
	return b
}

var streamID = uuid.NewString()

func notifyChunk(w StreamWriter, message string) {
	chunk := buildChunk(streamID, message, false, false)
	w.Write([]byte("data: " + string(chunk) + "\n\n"))
	w.Flush()
}

func notifyEnd(w StreamWriter, message string) {
	chunk := buildChunk(streamID, message, false, true)
	w.Write([]byte("data: " + string(chunk) + "\n\n"))
	w.Write([]byte("data: [DONE]\n\n"))
	w.Flush()
}

func agentAnswerStream(userQuestion string, w StreamWriter) {
	searchQueue := []string{userQuestion}
	seenIssues := make(map[string]map[string]interface{})
	var attemptQueries []string

	w.Write([]byte("data: {\"id\": \"" + streamID + "\", \"object\": \"chat.completion.chunk\", \"created\": " + fmt.Sprint(time.Now().Unix()) + ", \"model\": \"" + modelName + "\", \"choices\": [{\"index\": 0, \"delta\": {\"role\": \"assistant\"}}]}\n\n"))
	w.Flush()

	notifyChunk(w, "<think>")

	for step := 1; step <= maxSteps; step++ {
		if len(searchQueue) == 0 {
			break
		}
		query := searchQueue[0]
		searchQueue = searchQueue[1:]
		notifyChunk(w, fmt.Sprintf("Ищу в Youtrack: `%s`\n", query))

		params := map[string]string{
			"query":  query,
			"fields": "idReadable,summary,description,customFields(name,value(name))",
		}
		rawIssues, searchErr := youtrackRequest("/api/issues", params)

		notifyChunk(w, fmt.Sprintf("Найдено %d результатов.\n", len(rawIssues)))
		if len(rawIssues) > youtrackSearchLimit {
			rawIssues = rawIssues[0:youtrackSearchLimit]
		}
		for _, it := range rawIssues {
			id := it["idReadable"].(string)
			summary := it["summary"]
			description := it["description"]

			// Extract custom fields
			state, assignee, priority := "", "", ""
			var reviewers []string

			if cfList, ok := it["customFields"].([]interface{}); ok {
				for _, cf := range cfList {
					if cfMap, ok := cf.(map[string]interface{}); ok {
						name := cfMap["name"].(string)
						value := cfMap["value"]

						switch name {
						case "State":
							if vmap, ok := value.(map[string]interface{}); ok {
								state = vmap["name"].(string)
							}
						case "Assignee":
							if vmap, ok := value.(map[string]interface{}); ok {
								assignee = vmap["name"].(string)
							}
						case "Priority":
							if vmap, ok := value.(map[string]interface{}); ok {
								priority = vmap["name"].(string)
							}
						case "Reviewers":
							if vlist, ok := value.([]interface{}); ok {
								for _, v := range vlist {
									if user, ok := v.(map[string]interface{}); ok {
										reviewers = append(reviewers, user["name"].(string))
									}
								}
							}
						}
					}
				}
			}

			seenIssues[id] = map[string]interface{}{
				"id":          id,
				"title":       summary,
				"description": description,
				"link":        fmt.Sprintf("%s/issue/%s", youtrackURL, id),
				"state":       state,
				"assignee":    assignee,
				"priority":    priority,
				"reviewers":   reviewers,
			}
		}
		var issuesSlice []map[string]interface{}
		for _, v := range seenIssues {
			issuesSlice = append(issuesSlice, v)
		}
		issuesJSON, _ := json.Marshal(issuesSlice)
		expertUserPrompt := fmt.Sprintf("Вопрос пользователя: \"%s\"\n\nНайденные задачи:\n%s", userQuestion, issuesJSON)
		if len(attemptQueries) > 0 {
			expertUserPrompt += "Уже опробованные Youtrack-запросы: \n"
			for _, atemptQuery := range attemptQueries {
				expertUserPrompt += fmt.Sprintf("`%s`\n", atemptQuery)
			}
		}
		if searchErr != nil {
			expertUserPrompt += " Ошибка от последнего запроса: " + searchErr.Error()
		}
		if step == maxSteps {
			expertUserPrompt += " Это была последняя попытка что-либо поискать. Выдавай финальный ответ на основе тех задач, что были найдены."
		}
		attemptQueries = append(attemptQueries, query)
		notifyChunk(w, "Думаю...")

		reasoning, err := llmChat([]map[string]string{
			{"role": "system", "content": expertSystemPrompt},
			{"role": "user", "content": expertUserPrompt},
		}, 32000, 0, nil)
		if err != nil {
			break
		}
		reasoning = strings.TrimSpace(removeThinkTags(reasoning))
		if step == maxSteps {
			notifyChunk(w, "</think>")
			notifyEnd(w, reasoning+"\n")
			return
		} else {
			notifyChunk(w, reasoning+"\n")
		}

		assistantUserPrompt := fmt.Sprintf("/no_think Рассуждения эксперта: \"%s\"", reasoning)
		responseFormat := map[string]interface{}{
			"type": "json_schema",
			"json_schema": map[string]interface{}{
				"name":   "agent_action",
				"strict": true,
				"schema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"reasoning": map[string]interface{}{"type": "string"},
						"action":    map[string]interface{}{"type": "string"},
						"answer":    map[string]interface{}{"type": "string"},
						"query":     map[string]interface{}{"type": "string"},
					},
					"required": []string{"action", "answer"},
				},
			},
		}
		decision, err := llmChat([]map[string]string{
			{"role": "system", "content": assistantSystemPrompt},
			{"role": "user", "content": assistantUserPrompt},
		}, 32000, 0, responseFormat)
		if err != nil {
			break
		}
		var action struct {
			Action string `json:"action"`
			Answer string `json:"answer"`
			Query  string `json:"query"`
		}
		err = json.Unmarshal([]byte(decision), &action)
		if err != nil {
			continue
		}
		if action.Action == "final_answer" {
			notifyChunk(w, "</think>")
			notifyEnd(w, action.Answer+"\n")
			return
		}
		if action.Action == "yt_search" && action.Query != "" {
			if action.Answer != "" {
				notifyChunk(w, action.Answer+"\n")
			}
			searchQueue = append(searchQueue, action.Query)
			continue
		}
		break
	}
	notifyChunk(w, "</think>")
	notifyEnd(w, "Не получилось найти ответ\n")
}

func main() {
	flag.StringVar(&youtrackURL, "youtrack-url", "", "Base URL for YouTrack (required)")
	flag.StringVar(&youtrackToken, "youtrack-token", "", "YouTrack personal access token (required)")
	flag.StringVar(&llmURL, "llm-url", "", "URL for local LLM API (required)")
	flag.StringVar(&llmAPIKey, "llm-api-key", "", "API key for LLM access (required)")
	flag.StringVar(&modelName, "model-name", "", "Model name to use with LLM (required)")
	flag.IntVar(&maxSteps, "max-steps", -1, "Maximum number of reasoning steps (required)")
	flag.IntVar(&youtrackSearchLimit, "youtrack-search-limit", -1, "Maximum number of YouTrack search results to retrieve (required)")

	flag.Parse()

	missing := false

	if youtrackURL == "" {
		log.Println("Missing required flag: -youtrack-url")
		missing = true
	}
	if youtrackToken == "" {
		log.Println("Missing required flag: -youtrack-token")
		missing = true
	}
	if llmURL == "" {
		log.Println("Missing required flag: -llm-url")
		missing = true
	}
	if llmAPIKey == "" {
		log.Println("Missing required flag: -llm-api-key")
		missing = true
	}
	if modelName == "" {
		log.Println("Missing required flag: -model-name")
		missing = true
	}
	if maxSteps < 0 {
		log.Println("Missing or invalid required flag: -max-steps")
		missing = true
	}
	if youtrackSearchLimit < 0 {
		log.Println("Missing or invalid required flag: -youtrack-search-limit")
		missing = true
	}

	if missing {
		log.Fatal("One or more required flags are missing or invalid. Exiting.")
	}

	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil || len(req.Messages) == 0 {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		userPrompt := req.Messages[len(req.Messages)-1].Content
		agentAnswerStream(userPrompt, wrappedWriter{
			w: w,
			f: w.(http.Flusher),
			c: w.(http.CloseNotifier),
		})
	})
	http.ListenAndServe(":8082", nil)
}

func llmChat(messages []map[string]string, maxTokens int, temperature float64, responseFormat map[string]interface{}) (string, error) {
	payload := map[string]interface{}{
		"model":           modelName,
		"messages":        messages,
		"max_tokens":      maxTokens,
		"temperature":     temperature,
		"response_format": responseFormat,
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", llmURL+"/v1/chat/completions", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+llmAPIKey)
	req.Header.Set("Content-Type", "application/json")
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Transport: tr}
	client.Timeout = time.Minute * 2

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return "", err
	}
	choices := result["choices"].([]interface{})
	msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})["content"].(string)
	return msg, nil
}

func youtrackRequest(path string, params map[string]string) ([]map[string]interface{}, error) {
	baseURL, _ := url.Parse(youtrackURL + path)
	query := url.Values{}
	for k, v := range params {
		query.Set(k, v)
	}
	baseURL.RawQuery = query.Encode()

	req, _ := http.NewRequest("GET", baseURL.String(), nil)
	req.Header.Set("Authorization", "Bearer "+youtrackToken)
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Transport: tr}
	client.Timeout = time.Second * 15

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data []map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&data)
	return data, err
}

func removeThinkTags(input string) string {
	re := regexp.MustCompile(`(?s)<think>.*?</think>`)
	return re.ReplaceAllString(input, "")
}
