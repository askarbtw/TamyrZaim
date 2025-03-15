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

// Constants for state management
const (
	// Operation types
	OpAddLoan = "addloan"
	OpRepay   = "repay"
	OpNone    = ""

	// Menu callback data
	MenuAddLoan = "menu_addloan"
	MenuRepay   = "menu_repay"
	MenuBalance = "menu_balance"
	MenuStats   = "menu_stats"
)

// UserState manages the state for a single user
type UserState struct {
	Operation   string
	Step        int
	Data        map[string]string
	LastUpdated time.Time
}

// State manager for all users
type BotManager struct {
	bot             *tgbotapi.BotAPI
	db              *sql.DB
	userStates      map[int64]*UserState
	mu              sync.RWMutex
	lastProcessedID int
}

// Initialize a new bot manager
func NewBotManager(bot *tgbotapi.BotAPI, db *sql.DB) *BotManager {
	return &BotManager{
		bot:        bot,
		db:         db,
		userStates: make(map[int64]*UserState),
	}
}

// GetState returns the current state for a user, creating one if it doesn't exist
func (m *BotManager) GetState(chatID int64) *UserState {
	m.mu.RLock()
	state, exists := m.userStates[chatID]
	m.mu.RUnlock()

	if !exists {
		state = &UserState{
			Operation:   OpNone,
			Step:        0,
			Data:        make(map[string]string),
			LastUpdated: time.Now(),
		}
		m.mu.Lock()
		m.userStates[chatID] = state
		m.mu.Unlock()
	}

	return state
}

// SetState updates a user's state
func (m *BotManager) SetState(chatID int64, operation string, step int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, exists := m.userStates[chatID]
	if !exists {
		state = &UserState{
			Data: make(map[string]string),
		}
		m.userStates[chatID] = state
	}

	state.Operation = operation
	state.Step = step
	state.LastUpdated = time.Now()
}

// ClearState resets a user's state
func (m *BotManager) ClearState(chatID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.userStates, chatID)
}

// SaveStateData saves data to the user's state
func (m *BotManager) SaveStateData(chatID int64, key, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, exists := m.userStates[chatID]
	if !exists {
		return // Should never happen, but just in case
	}

	state.Data[key] = value
}

// SendMessage is a helper to send text messages
func (m *BotManager) SendMessage(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	_, err := m.bot.Send(msg)
	if err != nil {
		log.Printf("Error sending message: %v", err)
	}
}

// ShowMainMenu displays the main menu keyboard
func (m *BotManager) ShowMainMenu(chatID int64) {
	menuButtons := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💰 Записать займ", MenuAddLoan),
			tgbotapi.NewInlineKeyboardButtonData("✅ Записать возврат", MenuRepay),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Баланс", MenuBalance),
			tgbotapi.NewInlineKeyboardButtonData("📈 Статистика", MenuStats),
		),
	)

	msg := tgbotapi.NewMessage(chatID, "🤖 Выберите действие:")
	msg.ReplyMarkup = menuButtons
	_, err := m.bot.Send(msg)
	if err != nil {
		log.Printf("Error showing main menu: %v", err)
	}
}

// StartAddLoanFlow begins the process of recording a new loan
func (m *BotManager) StartAddLoanFlow(chatID int64) {
	// First clear any existing state
	m.ClearState(chatID)

	// Send the initial prompt
	m.SendMessage(chatID, "📝 Давайте запишем новый займ.\n👤 Введите имя заемщика:")

	// Then set the new state
	m.SetState(chatID, OpAddLoan, 0)

	log.Printf("Started add loan flow for user %d", chatID)
}

// StartRepayFlow begins the process of recording a loan repayment
func (m *BotManager) StartRepayFlow(chatID int64) {
	// First clear any existing state
	m.ClearState(chatID)

	// Send the initial prompt
	m.SendMessage(chatID, "💵 Давайте запишем возврат.\n🔢 Введите ID займа, который нужно отметить как возвращенный:")

	// Then set the new state
	m.SetState(chatID, OpRepay, 0)

	log.Printf("Started repay flow for user %d", chatID)
}

// HandleAddLoanStep processes each step of the add loan flow
func (m *BotManager) HandleAddLoanStep(chatID int64, text string) {
	state := m.GetState(chatID)

	log.Printf("Handling add loan step %d for user %d with input: %s", state.Step, chatID, text)

	switch state.Step {
	case 0: // Getting borrower name
		if text == "" {
			m.SendMessage(chatID, "❌ Имя заемщика не может быть пустым. Пожалуйста, введите корректное имя:")
			return
		}

		// Save borrower name and move to next step
		m.SaveStateData(chatID, "borrower_name", text)
		m.SetState(chatID, OpAddLoan, 1)
		m.SendMessage(chatID, "💰 Введите сумму займа:")

	case 1: // Getting loan amount
		amount, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			m.SendMessage(chatID, "❌ Некорректная сумма. Пожалуйста, введите целое число:")
			return
		}

		// Save amount and move to next step
		m.SaveStateData(chatID, "amount", fmt.Sprintf("%d", amount))
		m.SetState(chatID, OpAddLoan, 2)
		m.SendMessage(chatID, "📝 Введите цель займа:")

	case 2: // Getting loan purpose
		if text == "" {
			m.SendMessage(chatID, "❌ Цель займа не может быть пустой. Пожалуйста, введите корректную цель:")
			return
		}

		// Save purpose and complete the process
		m.SaveStateData(chatID, "purpose", text)

		// Generate a new loan ID
		var newLoanID int
		err := m.db.QueryRow("SELECT COALESCE(MAX(loan_id), 0) + 1 FROM loans WHERE user_id = ?", chatID).Scan(&newLoanID)
		if err != nil {
			log.Printf("Error generating loan ID: %v", err)
			m.SendMessage(chatID, fmt.Sprintf("❌ Ошибка при создании ID займа: %v", err))
			return
		}

		// Insert the new loan into the database
		query := `INSERT INTO loans (user_id, loan_id, borrower_name, amount, purpose, repaid) 
				  VALUES (?, ?, ?, ?, ?, 0)`
		_, err = m.db.Exec(
			query,
			chatID,
			newLoanID,
			state.Data["borrower_name"],
			state.Data["amount"],
			state.Data["purpose"],
		)

		if err != nil {
			log.Printf("Error inserting loan: %v", err)
			m.SendMessage(chatID, fmt.Sprintf("❌ Не удалось зарегистрировать займ: %v", err))
			return
		}

		// Send success message
		successMsg := fmt.Sprintf(
			"✅ Займ успешно зарегистрирован!\n\n"+
				"👤 Заемщик: %s\n"+
				"💰 Сумма: %s ₸\n"+
				"🎯 Цель: %s\n"+
				"🆔 ID займа: %d\n\n"+
				"〰️〰️〰️〰️〰️〰️〰️〰️〰️〰️",
			state.Data["borrower_name"],
			state.Data["amount"],
			state.Data["purpose"],
			newLoanID,
		)
		m.SendMessage(chatID, successMsg)

		// Clear state and show main menu
		m.ClearState(chatID)
		m.ShowMainMenu(chatID)
	}
}

// HandleRepayStep processes each step of the repay flow
func (m *BotManager) HandleRepayStep(chatID int64, text string) {
	state := m.GetState(chatID)

	log.Printf("Handling repay step %d for user %d with input: %s", state.Step, chatID, text)

	switch state.Step {
	case 0: // Getting loan ID
		if text == "" {
			m.SendMessage(chatID, "❌ Пожалуйста, введите корректный ID займа:")
			return
		}

		// Validate loan ID
		id, err := strconv.Atoi(text)
		if err != nil {
			m.SendMessage(chatID, "❌ Некорректный ID займа. Пожалуйста, введите корректное число:")
			return
		}

		// Check if loan exists and belongs to user
		var exists bool
		err = m.db.QueryRow(
			"SELECT EXISTS(SELECT 1 FROM loans WHERE loan_id = ? AND user_id = ? AND repaid = 0)",
			id, chatID,
		).Scan(&exists)

		if err != nil || !exists {
			log.Printf("Loan ID %d not found for user %d", id, chatID)
			m.SendMessage(chatID, "❌ Займ не найден или уже был возвращен. Пожалуйста, попробуйте снова с корректным ID займа.")
			m.ClearState(chatID)
			m.ShowMainMenu(chatID)
			return
		}

		// Get loan details for confirmation
		var borrowerName string
		var amount int64
		err = m.db.QueryRow(
			"SELECT borrower_name, amount FROM loans WHERE loan_id = ? AND user_id = ?",
			id, chatID,
		).Scan(&borrowerName, &amount)

		// Mark loan as repaid
		_, err = m.db.Exec("UPDATE loans SET repaid = 1 WHERE loan_id = ? AND user_id = ?", id, chatID)
		if err != nil {
			log.Printf("Error marking loan %d as repaid: %v", id, err)
			m.SendMessage(chatID, fmt.Sprintf("❌ Не удалось отметить займ как возвращенный: %v", err))
			return
		}

		// Send confirmation
		if err == nil {
			confirmMsg := fmt.Sprintf(
				"📌 Подтверждаем возврат займа:\n\n"+
					"🆔 ID займа: %d\n"+
					"👤 Заемщик: %s\n"+
					"💰 Сумма: %d ₸\n\n"+
					"✅ Займ отмечен как возвращенный!",
				id, borrowerName, amount,
			)
			m.SendMessage(chatID, confirmMsg)
		} else {
			m.SendMessage(chatID, fmt.Sprintf("✅ Займ с ID %d отмечен как возвращенный!", id))
		}

		// Clear state and show main menu
		m.ClearState(chatID)
		m.ShowMainMenu(chatID)
	}
}

// ShowBalance displays the user's active loans
func (m *BotManager) ShowBalance(chatID int64) {
	// Query active loans
	rows, err := m.db.Query(
		"SELECT loan_id, borrower_name, amount FROM loans WHERE user_id = ? AND repaid = 0",
		chatID,
	)

	if err != nil {
		log.Printf("Error querying loans: %v", err)
		m.SendMessage(chatID, fmt.Sprintf("❌ Ошибка при получении баланса: %v", err))
		return
	}
	defer rows.Close()

	// Build response
	var response strings.Builder
	response.WriteString("📊 Активные займы:\n\n")

	var totalAmount int64
	loanCount := 0

	// Process each loan
	for rows.Next() {
		var id int
		var borrower string
		var amount int64

		if err := rows.Scan(&id, &borrower, &amount); err != nil {
			log.Printf("Error scanning loan row: %v", err)
			continue
		}

		totalAmount += amount
		loanCount++

		response.WriteString(fmt.Sprintf(
			"🆔 Займ #%d\n👤 Заемщик: %s\n💰 Сумма: %d ₸\n➖➖➖➖➖➖➖➖➖➖\n\n",
			id, borrower, amount,
		))
	}

	// Add summary
	if loanCount == 0 {
		response.WriteString("У вас нет активных займов! 🎉")
	} else {
		response.WriteString(fmt.Sprintf("💼 Общая сумма активных займов: %d ₸", totalAmount))
	}

	// Send response
	m.SendMessage(chatID, response.String())
}

// ShowStats displays lending statistics
func (m *BotManager) ShowStats(chatID int64) {
	var totalLoans int
	var totalLent int64
	var totalRepaid int

	// Get total loans and amount
	err := m.db.QueryRow(
		"SELECT COUNT(*), COALESCE(SUM(amount), 0) FROM loans WHERE user_id = ?",
		chatID,
	).Scan(&totalLoans, &totalLent)

	if err != nil {
		log.Printf("Error getting loan stats: %v", err)
		m.SendMessage(chatID, fmt.Sprintf("❌ Ошибка при формировании статистики: %v", err))
		return
	}

	// Get repaid count
	err = m.db.QueryRow(
		"SELECT COUNT(*) FROM loans WHERE user_id = ? AND repaid = 1",
		chatID,
	).Scan(&totalRepaid)

	if err != nil {
		log.Printf("Error getting repaid count: %v", err)
		m.SendMessage(chatID, fmt.Sprintf("❌ Ошибка при формировании статистики: %v", err))
		return
	}

	// Format stats message
	stats := fmt.Sprintf(
		"📈 Статистика займов:\n\n"+
			"🔢 Всего займов: %d\n"+
			"💰 Всего выдано: %d ₸\n"+
			"✅ Возвращено займов: %d\n"+
			"⏳ Ожидают возврата: %d\n\n"+
			"〰️〰️〰️〰️〰️〰️〰️〰️〰️〰️",
		totalLoans,
		totalLent,
		totalRepaid,
		totalLoans-totalRepaid,
	)

	// Send stats
	m.SendMessage(chatID, stats)
}

// HandleCallbackQuery processes button presses
func (m *BotManager) HandleCallbackQuery(query *tgbotapi.CallbackQuery) {
	chatID := query.Message.Chat.ID
	data := query.Data

	log.Printf("Callback query from user %d: %s", chatID, data)

	// Acknowledge the button press
	m.bot.Request(tgbotapi.NewCallback(query.ID, ""))

	// Remove the keyboard to prevent multiple clicks
	edit := tgbotapi.NewEditMessageText(chatID, query.Message.MessageID, query.Message.Text)
	edit.ReplyMarkup = nil
	m.bot.Send(edit)

	// Handle different actions
	switch data {
	case MenuAddLoan:
		m.StartAddLoanFlow(chatID)
	case MenuRepay:
		m.StartRepayFlow(chatID)
	case MenuBalance:
		m.ClearState(chatID)
		m.ShowBalance(chatID)
		m.ShowMainMenu(chatID)
	case MenuStats:
		m.ClearState(chatID)
		m.ShowStats(chatID)
		m.ShowMainMenu(chatID)
	default:
		log.Printf("Unknown callback data: %s", data)
		m.SendMessage(chatID, "❌ Неизвестная опция. Попробуйте снова.")
		m.ShowMainMenu(chatID)
	}
}

// HandleMessage processes text messages
func (m *BotManager) HandleMessage(message *tgbotapi.Message) {
	chatID := message.Chat.ID
	text := strings.TrimSpace(message.Text)

	log.Printf("Message from user %d: %s", chatID, text)

	// Handle commands
	if message.IsCommand() {
		switch message.Command() {
		case "start":
			m.ClearState(chatID)
			m.ShowMainMenu(chatID)
		default:
			m.SendMessage(chatID, "🤔 Неизвестная команда. Используйте /start для начала работы.")
		}
		return
	}

	// Handle conversation state
	state := m.GetState(chatID)

	switch state.Operation {
	case OpAddLoan:
		m.HandleAddLoanStep(chatID, text)
	case OpRepay:
		m.HandleRepayStep(chatID, text)
	case OpNone: // No active conversation
		m.ShowMainMenu(chatID)
	default:
		log.Printf("Unknown operation: %s", state.Operation)
		m.ShowMainMenu(chatID)
	}
}

// StartReminderScheduler sends weekly reminders about outstanding loans
func (m *BotManager) StartReminderScheduler() {
	go func() {
		ticker := time.NewTicker(7 * 24 * time.Hour)
		for {
			<-ticker.C
			m.SendReminders()
		}
	}()
}

// SendReminders sends reminder messages to users with outstanding loans
func (m *BotManager) SendReminders() {
	// Get distinct users with active loans
	rows, err := m.db.Query("SELECT DISTINCT user_id FROM loans WHERE repaid = 0")
	if err != nil {
		log.Printf("Error querying users for reminders: %v", err)
		return
	}
	defer rows.Close()

	// Build list of users
	var userIDs []int64
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			log.Printf("Error scanning user ID: %v", err)
			continue
		}
		userIDs = append(userIDs, userID)
	}

	// Send reminders to each user
	for _, userID := range userIDs {
		// Get active loans for this user
		loanRows, err := m.db.Query(
			"SELECT loan_id, borrower_name, amount FROM loans WHERE user_id = ? AND repaid = 0",
			userID,
		)
		if err != nil {
			log.Printf("Error querying loans for user %d: %v", userID, err)
			continue
		}

		// Build reminder message
		reminderMsg := "⏰ Еженедельное напоминание: У вас есть активные займы:\n\n"

		for loanRows.Next() {
			var id int
			var borrower string
			var amount int64

			if err := loanRows.Scan(&id, &borrower, &amount); err != nil {
				log.Printf("Error scanning loan: %v", err)
				continue
			}

			reminderMsg += fmt.Sprintf("🆔 Займ #%d - %s: %d ₸\n", id, borrower, amount)
		}
		loanRows.Close()

		// Send the reminder
		m.SendMessage(userID, reminderMsg)
	}
}

// Start runs the bot and begins processing updates
func (m *BotManager) Start() {
	log.Println("Starting bot...")

	// Configure update channel
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := m.bot.GetUpdatesChan(u)

	// Start reminder scheduler
	m.StartReminderScheduler()

	// Process updates
	for update := range updates {
		// Skip already processed updates
		if update.UpdateID <= m.lastProcessedID {
			continue
		}
		m.lastProcessedID = update.UpdateID

		// Process callback queries (button presses)
		if update.CallbackQuery != nil {
			m.HandleCallbackQuery(update.CallbackQuery)
			continue
		}

		// Process messages
		if update.Message != nil && update.Message.Text != "" {
			m.HandleMessage(update.Message)
		}
	}
}

// Initialize database schema
func initializeDatabase(db *sql.DB) error {
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS loans (
		user_id INTEGER NOT NULL,
		loan_id INTEGER NOT NULL,
		borrower_name TEXT NOT NULL,
		amount INTEGER NOT NULL,
		purpose TEXT,
		repaid BOOLEAN DEFAULT 0,
		PRIMARY KEY (user_id, loan_id)
	);`

	_, err := db.Exec(createTableSQL)
	return err
}

func main() {
	// Get bot token from environment
	botToken := os.Getenv("BOT_TOKEN")
	if botToken == "" {
		log.Fatal("BOT_TOKEN environment variable not set")
	}

	// Initialize Telegram bot
	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Fatalf("Failed to initialize bot: %v", err)
	}
	log.Printf("Authorized as @%s", bot.Self.UserName)

	// Open database connection
	db, err := sql.Open("sqlite", "./lending.db")
	if err != nil {
		log.Fatalf("Error opening database: %v", err)
	}
	defer db.Close()

	// Initialize database schema
	if err := initializeDatabase(db); err != nil {
		log.Fatalf("Error initializing database: %v", err)
	}

	// Create and start bot manager
	manager := NewBotManager(bot, db)
	manager.Start()
}
