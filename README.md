# Telegram to Obsidian Exporter

A Go application for exporting messages from Telegram chats/channels to Markdown format suitable for Obsidian, including downloading, optimizing, and linking media files and preserving reply relationships.

## Features

- Export messages from specified Telegram chats, channels, or groups to Markdown
- Download and optimize media files (images, videos, audio, documents)
- Create Obsidian-compatible links to media files
- Maintain reply relationships between messages
- Interactive mode for selecting export targets
- Resume exports with only new messages
- Configurable media optimization (quality, codecs, hardware acceleration)
- Detailed logging and progress tracking

## Installation

1. Clone the repository:

```bash
git clone https://github.com/abshka/telegram-obsidian-go.git
cd telegram-obsidian
```

2. Install dependencies:

```bash
go mod download
```

3. Create a `.env` file with your configuration:

```bash
cp .env.example .env
# Edit the .env file with your API credentials and settings
```

## Getting Telegram API Credentials

To use the Telegram API, you need to obtain your `APP_ID` and `APP_HASH`:

1. Visit [my.telegram.org](https://my.telegram.org)
2. Log in with your Telegram account
3. Go to "API development tools"
4. Fill out the form to create an application
5. Copy the provided `api_id` and `api_hash` to your `.env` file

## Configuration

Edit the `.env` file to set the following parameters:

### Required settings

```
APP_ID=your_api_id
APP_HASH=your_api_hash
PHONE_NUMBER=+your_phone_number
OBSIDIAN_PATH=/path/to/your/obsidian/vault
```

### Export targets

```
# Comma-separated list of Telegram channel IDs, usernames, or links
EXPORT_TARGETS=@username,123456789,https://t.me/channelname
```

### Optional settings

See `.env.example` for all available configuration options and their default values.

## Usage

```bash
go run main.go
```

On first run, the application will ask for your phone number and the confirmation code you'll receive in your Telegram app. If your account has two-factor authentication enabled, it will also ask for your password.

After successful authentication, the application will:

1. Connect to the Telegram API
2. If in interactive mode, allow you to select export targets
3. For each target, fetch messages (optionally only new ones)
4. Download and optimize media files
5. Generate Markdown notes
6. Create links between replies

## Building from Source

To build a standalone executable:

```bash
go build -o telegram-obsidian main.go
```

## Obsidian Integration

The exported notes will be saved in your Obsidian vault with the following structure:

```
obsidian_vault/
├── [entity_folder_1]/   (if USE_ENTITY_FOLDERS=true)
│   ├── 2023/            (year folders)
│   │   ├── 2023-01-01_Message_title.md
│   │   └── ...
│   └── ...
├── [entity_folder_2]/
│   └── ...
└── media/               (or custom MEDIA_SUBDIR)
    ├── [entity_folder_1]/
    │   ├── entity1_msg1234_photo_1.webp
    │   └── ...
    └── [entity_folder_2]/
        └── ...
```

Each note will contain:

- Message text (with Markdown-escaped characters)
- Media embeds using Obsidian's `![[...]]` syntax
- Reply links to related messages

## License

MIT
