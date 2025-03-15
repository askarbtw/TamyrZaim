package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	_ "modernc.org/sqlite"
)

// Conversation holds details for multi-step interactions.
type Conversation struct {
	Operation string            // "addloan" or "repay"
	Step      int               // Step number in the conversation
	Data      map[string]string // Temporary data storage
}

// Global conversation state map (keyed by chat ID)
var convMutex sync.Mutex
var conversations = make(map[int64]*Conversation)

// Global bot variable
var bot *tgbotapi.BotAPI

func main() {
	//err := godotenv.Load()
	//if err != nil {
	//	log.Fatal("Error loading .env file")
	//}

	// Get BOT_TOKEN from environment variable
	botToken := os.Getenv("BOT_TOKEN")
	if botToken == "" {
		log.Fatal("BOT_TOKEN environment variable not set.")
	}

	// ✅ Initialize the Telegram bot
	var errBot error
	bot, errBot = tgbotapi.NewBotAPI(botToken) // <== This was missing
	if errBot != nil {
		log.Fatalf("Failed to initialize bot: %v", errBot)
	}

	log.Println("Bot token loaded successfully")

	// Open SQLite database.
	db, err := sql.Open("sqlite", "./lending.db")
	if err != nil {
		log.Fatalf("Error opening DB: %v", err)
	}
	defer db.Close()

	// Create loans table using the updated schema.
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS loans (
    user_id INTEGER NOT NULL,
    loan_id INTEGER NOT NULL,
    borrower_name TEXT NOT NULL,
    amount REAL NOT NULL,
    purpose TEXT,
    repaid BOOLEAN DEFAULT 0,
    PRIMARY KEY (user_id, loan_id)
);`
	_, err = db.Exec(createTableSQL)
	if err != nil {
		log.Fatalf("Error creating table: %v", err)
	}

	// Start weekly reminder scheduler.
	go reminderScheduler(bot, db)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	// Track last processed update ID to prevent duplicates
	var lastUpdateID int

	for update := range updates {
		// Skip already processed updates to prevent duplicates
		if update.UpdateID <= lastUpdateID {
			continue
		}
		lastUpdateID = update.UpdateID

		if update.CallbackQuery != nil {
			handleCallbackQuery(bot, db, update.CallbackQuery)
			continue
		}

		if update.Message == nil || update.Message.Text == "" {
			continue
		}

		handleMessage(bot, db, update.Message)
	}
}

// handleMessage processes incoming messages.
func handleMessage(bot *tgbotapi.BotAPI, db *sql.DB, message *tgbotapi.Message) {
	chatID := message.Chat.ID
	text := strings.TrimSpace(message.Text)

	log.Printf("Received message from %d: %s", chatID, text)

	if message.IsCommand() {
		switch message.Command() {
		case "start":
			// Clear any existing conversation when /start is used
			endConversation(chatID)
			sendMainMenu(bot, chatID)
			return
		default:
			sendMessage(bot, chatID, "🤔 Неизвестная команда. Используйте /start для начала работы.")
			return
		}
	}

	// Check if user is in an active conversation
	convMutex.Lock()
	_, conversationExists := conversations[chatID]
	convMutex.Unlock()

	if conversationExists {
		log.Printf("Active conversation exists for user %d, handling input", chatID)
		handleConversationInput(bot, db, chatID, text)
	} else {
		log.Printf("No active conversation for user %d, showing main menu", chatID)
		sendMainMenu(bot, chatID)
	}
}

// handleConversationInput routes user input to the correct conversation handler.
func handleConversationInput(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, text string) {
	convMutex.Lock()
	conv, exists := conversations[chatID]
	convMutex.Unlock()

	if !exists {
		log.Printf("Error: Conversation not found for user %d in handleConversationInput", chatID)
		sendMainMenu(bot, chatID)
		return
	}

	log.Printf("Processing conversation for user %d: operation=%s, step=%d", chatID, conv.Operation, conv.Step)

	switch conv.Operation {
	case "addloan":
		handleAddLoanStep(bot, db, chatID, text, conv)
	case "repay":
		handleRepayStep(bot, db, chatID, text, conv)
	default:
		log.Printf("Unknown operation: %s for user %d", conv.Operation, chatID)
		sendMainMenu(bot, chatID)
	}
}

// handleAddLoanStep processes each step of adding a loan.
func handleAddLoanStep(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, text string, conv *Conversation) {
	// Lock the conversation to prevent race conditions
	convMutex.Lock()
	defer convMutex.Unlock()

	// Double-check the conversation still exists
	if conv == nil {
		log.Printf("Error: Conversation is nil for user %d", chatID)
		sendMainMenu(bot, chatID)
		return
	}

	log.Printf("Processing add loan step %d for user %d with input: %s", conv.Step, chatID, text)

	switch conv.Step {
	case 0:
		// Ask for borrower's name.
		if text == "" {
			sendMessage(bot, chatID, "❌ Имя заемщика не может быть пустым. Пожалуйста, введите корректное имя:")
			return
		}
		conv.Data["borrower_name"] = text
		conv.Step++
		log.Printf("User %d provided borrower name: %s, moving to step 1", chatID, text)
		sendMessage(bot, chatID, "💰 Введите сумму займа (например, 100.50):")
	case 1:
		// Validate and save loan amount.
		amount, err := strconv.ParseFloat(text, 64)
		if err != nil {
			sendMessage(bot, chatID, "❌ Некорректная сумма. Пожалуйста, введите корректное число (например, 100.50):")
			return
		}
		conv.Data["amount"] = fmt.Sprintf("%.2f", amount)
		conv.Step++
		log.Printf("User %d provided loan amount: %.2f, moving to step 2", chatID, amount)
		sendMessage(bot, chatID, "📝 Введите цель займа:")
	case 2:
		// Save purpose and complete recording process.
		if text == "" {
			sendMessage(bot, chatID, "❌ Цель займа не может быть пустой. Пожалуйста, введите корректную цель:")
			return
		}
		conv.Data["purpose"] = text
		log.Printf("User %d provided loan purpose: %s", chatID, text)

		var newLoanID int
		err := db.QueryRow("SELECT COALESCE(MAX(loan_id), 0) + 1 FROM loans WHERE user_id = ?", chatID).Scan(&newLoanID)
		if err != nil {
			log.Printf("Error generating loan ID for user %d: %v", chatID, err)
			sendMessage(bot, chatID, fmt.Sprintf("❌ Ошибка при создании ID займа: %v", err))
			return
		}
		log.Printf("Generated new loan ID %d for user %d", newLoanID, chatID)

		// Insert the loan into the database.
		query := `INSERT INTO loans (user_id, loan_id, borrower_name, amount, purpose, repaid) VALUES (?, ?, ?, ?, ?, 0)`
		_, err = db.Exec(query, chatID, newLoanID, conv.Data["borrower_name"], conv.Data["amount"], conv.Data["purpose"])
		if err != nil {
			log.Printf("Error recording loan for user %d: %v", chatID, err)
			sendMessage(bot, chatID, fmt.Sprintf("❌ Не удалось зарегистрировать займ: %v", err))
			return
		}
		log.Printf("Successfully recorded loan ID %d for user %d", newLoanID, chatID)

		successMsg := fmt.Sprintf(
			"✅ Займ успешно зарегистрирован!\n\n"+
				"👤 Заемщик: %s\n"+
				"💰 Сумма: %s ₸\n"+
				"🎯 Цель: %s\n"+
				"🆔 ID займа: %d\n\n"+
				"〰️〰️〰️〰️〰️〰️〰️〰️〰️〰️",
			conv.Data["borrower_name"],
			conv.Data["amount"],
			conv.Data["purpose"],
			newLoanID,
		)
		sendMessage(bot, chatID, successMsg)

		// Delete conversation BEFORE sending main menu to prevent race conditions
		log.Printf("Ending add loan conversation for user %d", chatID)
		delete(conversations, chatID)
		sendMainMenu(bot, chatID)
	default:
		log.Printf("Unknown add loan step %d for user %d", conv.Step, chatID)
		sendMessage(bot, chatID, "❌ Произошла ошибка в процессе добавления займа. Пожалуйста, попробуйте снова.")
		delete(conversations, chatID)
		sendMainMenu(bot, chatID)
	}
}

// handleRepayStep processes each step of recording a repayment.
func handleRepayStep(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, text string, conv *Conversation) {
	// Lock the conversation to prevent race conditions
	convMutex.Lock()
	defer convMutex.Unlock()

	// Double-check the conversation still exists
	if conv == nil {
		log.Printf("Error: Conversation is nil for user %d", chatID)
		sendMainMenu(bot, chatID)
		return
	}

	log.Printf("Processing repay step %d for user %d with input: %s", conv.Step, chatID, text)

	switch conv.Step {
	case 0:
		// Ask for Loan ID to mark as repaid.
		if text == "" {
			sendMessage(bot, chatID, "❌ Пожалуйста, введите корректный ID займа:")
			return
		}
		conv.Data["loan_id"] = text
		id, err := strconv.Atoi(text)
		if err != nil {
			sendMessage(bot, chatID, "❌ Некорректный ID займа. Пожалуйста, введите корректное число:")
			return
		}
		log.Printf("User %d provided loan ID: %d for repayment", chatID, id)

		// Check if loan exists and belongs to user.
		var exists bool
		row := db.QueryRow("SELECT EXISTS(SELECT 1 FROM loans WHERE loan_id = ? AND user_id = ? AND repaid = 0)", id, chatID)
		err = row.Scan(&exists)
		if err != nil || !exists {
			log.Printf("Loan ID %d not found or already repaid for user %d", id, chatID)
			sendMessage(bot, chatID, "❌ Займ не найден или уже был возвращен. Пожалуйста, попробуйте снова с корректным ID займа.")
			delete(conversations, chatID)
			sendMainMenu(bot, chatID)
			return
		}

		// Get loan details for confirmation
		var borrowerName string
		var amount float64
		err = db.QueryRow("SELECT borrower_name, amount FROM loans WHERE loan_id = ? AND user_id = ?", id, chatID).Scan(&borrowerName, &amount)
		if err == nil {
			log.Printf("Found loan ID %d for user %d: borrower=%s, amount=%.2f", id, chatID, borrowerName, amount)

			// Mark the loan as repaid.
			_, err = db.Exec("UPDATE loans SET repaid = 1 WHERE loan_id = ? AND user_id = ?", id, chatID)
			if err != nil {
				log.Printf("Error marking loan %d as repaid: %v", id, err)
				sendMessage(bot, chatID, fmt.Sprintf("❌ Не удалось отметить займ как возвращенный: %v", err))
				return
			}

			confirmMsg := fmt.Sprintf(
				"📌 Подтверждаем возврат займа:\n\n"+
					"🆔 ID займа: %d\n"+
					"👤 Заемщик: %s\n"+
					"💰 Сумма: %.2f ₸\n\n"+
					"✅ Займ отмечен как возвращенный!",
				id, borrowerName, amount,
			)
			sendMessage(bot, chatID, confirmMsg)
		} else {
			log.Printf("Error getting loan details for ID %d: %v", id, err)

			// Mark the loan as repaid even if we couldn't get details.
			_, err = db.Exec("UPDATE loans SET repaid = 1 WHERE loan_id = ? AND user_id = ?", id, chatID)
			if err != nil {
				log.Printf("Error marking loan %d as repaid: %v", id, err)
				sendMessage(bot, chatID, fmt.Sprintf("❌ Не удалось отметить займ как возвращенный: %v", err))
				return
			}
			sendMessage(bot, chatID, fmt.Sprintf("✅ Займ с ID %d отмечен как возвращенный!", id))
		}

		// Delete conversation BEFORE sending main menu
		log.Printf("Successfully marked loan %d as repaid for user %d", id, chatID)
		delete(conversations, chatID)
		sendMainMenu(bot, chatID)
	default:
		log.Printf("Unknown repay step %d for user %d", conv.Step, chatID)
		sendMessage(bot, chatID, "❌ Произошла ошибка в процессе регистрации возврата. Пожалуйста, попробуйте снова.")
		delete(conversations, chatID)
		sendMainMenu(bot, chatID)
	}
}

// endConversation clears the conversation state for a user.
func endConversation(chatID int64) {
	convMutex.Lock()
	delete(conversations, chatID)
	convMutex.Unlock()
}

// sendMainMenu displays the main inline keyboard menu.
func sendMainMenu(bot *tgbotapi.BotAPI, chatID int64) {
	menuButtons := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💰 Записать займ", "menu_addloan"),
			tgbotapi.NewInlineKeyboardButtonData("✅ Записать возврат", "menu_repay"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Баланс", "menu_balance"),
			tgbotapi.NewInlineKeyboardButtonData("📈 Статистика", "menu_stats"),
		),
	)
	msg := tgbotapi.NewMessage(chatID, "🤖 Выберите действие:")
	msg.ReplyMarkup = menuButtons
	bot.Send(msg)
}

// sendMessage is a helper function to send messages.
func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	bot.Send(msg)
}

// handleCallbackQuery processes inline keyboard callback queries.
func handleCallbackQuery(bot *tgbotapi.BotAPI, db *sql.DB, cq *tgbotapi.CallbackQuery) {
	data := cq.Data
	chatID := cq.Message.Chat.ID

	log.Printf("Received callback query from %d: %s", chatID, data)

	// First respond to the callback to stop "loading" indicator
	bot.Request(tgbotapi.NewCallback(cq.ID, ""))

	// Edit the message that contained the inline keyboard to remove the keyboard
	// This prevents the user from clicking the buttons multiple times
	edit := tgbotapi.NewEditMessageText(chatID, cq.Message.MessageID, cq.Message.Text)
	edit.ReplyMarkup = nil
	bot.Send(edit)

	switch data {
	case "menu_addloan":
		// Clear any existing conversation before starting a new one
		endConversation(chatID)
		startAddLoanConversation(chatID)
	case "menu_repay":
		// Clear any existing conversation before starting a new one
		endConversation(chatID)
		startRepayConversation(chatID)
	case "menu_balance":
		// No need for conversation for balance
		endConversation(chatID)
		showBalance(bot, db, chatID)
		// Show main menu again after displaying balance
		sendMainMenu(bot, chatID)
	case "menu_stats":
		// No need for conversation for stats
		endConversation(chatID)
		showStats(bot, db, chatID)
		// Show main menu again after displaying stats
		sendMainMenu(bot, chatID)
	default:
		log.Printf("Unknown callback data: %s", data)
		sendMessage(bot, chatID, "❌ Неизвестная опция. Попробуйте снова.")
		sendMainMenu(bot, chatID)
	}
}

// startAddLoanConversation initializes a conversation for adding a loan.
func startAddLoanConversation(chatID int64) {
	// First send the initial prompt to request borrower name
	sendMessage(bot, chatID, "📝 Давайте запишем новый займ.\n👤 Введите имя заемщика:")

	// Then create the conversation - this order ensures the user sees the prompt first
	convMutex.Lock()
	conversations[chatID] = &Conversation{
		Operation: "addloan",
		Step:      0,
		Data:      make(map[string]string),
	}
	convMutex.Unlock()

	log.Printf("Started add loan conversation for user %d", chatID)
}

// startRepayConversation initializes a conversation for recording a repayment.
func startRepayConversation(chatID int64) {
	// First send the initial prompt to request loan ID
	sendMessage(bot, chatID, "💵 Давайте запишем возврат.\n🔢 Введите ID займа, который нужно отметить как возвращенный:")

	// Then create the conversation - this order ensures the user sees the prompt first
	convMutex.Lock()
	conversations[chatID] = &Conversation{
		Operation: "repay",
		Step:      0,
		Data:      make(map[string]string),
	}
	convMutex.Unlock()

	log.Printf("Started repay conversation for user %d", chatID)
}

// showBalance retrieves and displays the user's active loans.
func showBalance(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64) {
	rows, err := db.Query("SELECT loan_id, borrower_name, amount FROM loans WHERE user_id = ? AND repaid = 0", chatID)
	if err != nil {
		sendMessage(bot, chatID, fmt.Sprintf("❌ Ошибка при получении баланса: %v", err))
		return
	}
	defer rows.Close()

	var response strings.Builder
	response.WriteString("📊 Активные займы:\n\n")
	var totalAmount float64
	loanCount := 0

	for rows.Next() {
		var id int
		var borrower string
		var amount float64
		err := rows.Scan(&id, &borrower, &amount)
		if err != nil {
			continue
		}
		totalAmount += amount
		loanCount++
		response.WriteString(fmt.Sprintf("🆔 Займ #%d\n👤 Заемщик: %s\n💰 Сумма: %.2f ₸\n➖➖➖➖➖➖➖➖➖➖\n\n", id, borrower, amount))
	}

	if loanCount == 0 {
		response.WriteString("У вас нет активных займов! 🎉")
	} else {
		response.WriteString(fmt.Sprintf("💼 Общая сумма активных займов: %.2f ₸", totalAmount))
	}
	sendMessage(bot, chatID, response.String())
}

// showStats generates and displays lending statistics.
func showStats(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64) {
	var totalLoans int
	var totalLent float64
	var totalRepaid int

	err := db.QueryRow("SELECT COUNT(*), SUM(amount) FROM loans WHERE user_id = ?", chatID).Scan(&totalLoans, &totalLent)
	if err != nil {
		sendMessage(bot, chatID, fmt.Sprintf("❌ Ошибка при формировании статистики: %v", err))
		return
	}

	err = db.QueryRow("SELECT COUNT(*) FROM loans WHERE user_id = ? AND repaid = 1", chatID).Scan(&totalRepaid)
	if err != nil {
		sendMessage(bot, chatID, fmt.Sprintf("❌ Ошибка при формировании статистики: %v", err))
		return
	}

	stats := fmt.Sprintf(
		"📈 Статистика займов:\n\n"+
			"🔢 Всего займов: %d\n"+
			"💰 Всего выдано: %.2f ₸\n"+
			"✅ Возвращено займов: %d\n"+
			"⏳ Ожидают возврата: %d\n\n"+
			"〰️〰️〰️〰️〰️〰️〰️〰️〰️〰️",
		totalLoans,
		totalLent,
		totalRepaid,
		totalLoans-totalRepaid,
	)
	sendMessage(bot, chatID, stats)
}

// reminderScheduler sends weekly reminders to users with pending loans.
func reminderScheduler(bot *tgbotapi.BotAPI, db *sql.DB) {
	ticker := time.NewTicker(7 * 24 * time.Hour)
	for {
		<-ticker.C

		// Get distinct user IDs with pending loans.
		rows, err := db.Query("SELECT DISTINCT user_id FROM loans WHERE repaid = 0")
		if err != nil {
			log.Printf("Error querying users for reminders: %v", err)
			continue
		}
		var userIDs []int64
		for rows.Next() {
			var userID int64
			if err := rows.Scan(&userID); err != nil {
				continue
			}
			userIDs = append(userIDs, userID)
		}
		rows.Close()

		// For each user, compile a reminder message and send it.
		for _, userID := range userIDs {
			reminderMsg := "⏰ Еженедельное напоминание: У вас есть активные займы:\n\n"
			loanRows, err := db.Query("SELECT loan_id, borrower_name, amount FROM loans WHERE user_id = ? AND repaid = 0", userID)
			if err != nil {
				continue
			}
			for loanRows.Next() {
				var id int
				var borrower string
				var amount float64
				if err := loanRows.Scan(&id, &borrower, &amount); err != nil {
					continue
				}
				reminderMsg += fmt.Sprintf("🆔 Займ #%d - %s: %.2f ₸\n", id, borrower, amount)
			}
			loanRows.Close()
			sendMessage(bot, userID, reminderMsg)
		}
	}
}
