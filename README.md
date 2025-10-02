# O365 Mailbox Downloader

`o365mbx` is a command-line application written in Go to download emails and attachments from Microsoft Office 365 (O365) mailboxes using the Microsoft Graph API. It leverages the official Microsoft Graph SDK for Go to ensure reliable and efficient communication with the API.

It is designed for high-performance, parallelized downloading and is robust and efficient, with features for handling large mailboxes, large attachments, and transient network errors.

## Features

*   **High-Performance Parallel Processing**: Utilizes a decoupled producer-consumer architecture to download multiple messages and attachments concurrently, maximizing throughput within configurable API limits.
*   **Reliable Attachment Handling**: Uses a two-phase download strategy. It first fetches messages and then fetches attachments for each message individually. This approach improves reliability and reduces memory usage, preventing API timeouts when processing mailboxes with large numbers of attachments.
*   **Email and Attachment Download**: Downloads emails and their attachments from a specified O365 mailbox.
*   **HTML to Plain Text Conversion**: Cleans email bodies by converting HTML to plain text, preserving links and image alt text.
*   **Incremental Downloads**: Performs incremental downloads by saving the timestamp of the last run, fetching only new emails since that time.
*   **Robust Error Handling**: Implements custom error types for better error identification and handling.
*   **Configurable Retry Mechanism**: Uses the Microsoft Graph SDK's built-in retry mechanism to handle transient network errors and API rate limiting.
*   **Flexible Configuration**: Supports configuration via both a JSON file and command-line arguments, with arguments overriding file settings.
*   **Bandwidth Limiting**: Allows throttling of download bandwidth to avoid hitting data egress limits on large-scale downloads.
*   **Secure Token Management**: Provides multiple, mutually exclusive options for securely supplying the access token.
*   **Health Check Mode**: Provides a "health check" mode to verify connectivity and authentication with the O365 mailbox without performing a full download.
*   **Structured Logging**: Uses `logrus` for structured and informative logging, with a configurable debug level.
*   **Advanced PDF Conversion Control**: For users converting emails to PDF, the application provides a `-rod` flag. This flag allows passing launch arguments directly to the underlying `go-rod` headless browser instance. For example, to use a proxy, you could pass `-rod="--proxy-server=127.0.0.1:8080"`. This provides fine-grained control over the browser environment used for conversion.

## System Requirements

### File Path Length

The application now supports a maximum file path length of 512 characters. On systems where this limit might be an issue (e.g., older Windows versions without long path support), please see the requirements below.

### Windows Long Path Support

Due to the way email subjects and attachment names can create very long file paths, `o365mbx` requires that "Win32 long path" support is enabled on Windows systems. The application will check for this at startup and will exit with an error if it is not enabled.

You can enable this feature using one of the following methods:

**Using the Group Policy Editor (`gpedit.msc`):**
1.  Navigate to: `Local Computer Policy` -> `Computer Configuration` -> `Administrative Templates` -> `System` -> `Filesystem`.
2.  Find and enable the "Enable Win32 long paths" option.

**Using the Registry Editor (`regedit.exe`):**
1.  Navigate to: `HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Control\FileSystem`.
2.  Set the value of `LongPathsEnabled` (a `DWORD` type) to `1`.

A system restart may be required for the change to take effect.

## Workspace Directory Structure

The application saves each email into a dedicated folder within the specified workspace. The folder is named after the message's unique ID. Here is an example of the directory structure for a single downloaded email:

```
/path/to/your/workspace/
└── AAMkAGI0ZD... (message ID)
    ├── attachments
    │   ├── 01_quarterly_report.pdf
    │   └── 02_logo.png
    ├── body.html
    └── metadata.json
```

*   **`metadata.json`**: A JSON file containing detailed metadata about the email, including sender, recipients, subject, date, and information about the attachments.
*   **`body.html` / `body.txt` / `body.pdf`**: The body of the email. The extension depends on the original content type or the conversion option specified.
*   **`attachments/`**: A sub-directory containing all attachments from the email. Each attachment is prefixed with a two-digit sequence number.

## Metadata JSON Specifications

The `metadata.json` file provides a detailed overview of the downloaded email.

### Field Descriptions

*   **`to`**: A list of recipients in the "To" field. Each recipient object contains an `emailAddress` object with `name` and `address`.
*   **`cc`**: A list of recipients in the "Cc" field. Same structure as `to`.
*   **`from`**: The sender of the email. Same structure as a recipient object.
*   **`subject`**: The subject line of the email.
*   **`received_date`**: The date and time the email was received, in ISO 8601 format (UTC).
*   **`body`**: The filename of the email body (e.g., `body.html`, `body.txt` or `body.pdf`).
*   **`content_type_of_body`**: The content type of the saved body file (`text/html`, `text/plain`, or `application/pdf`).
*   **`attachment_counts`**: The total number of attachments in the email.
*   **`list_of_attachments`**: A list of objects, where each object represents an attachment and contains the following fields:
    *   `attachment_name_in_message`: The original filename of the attachment.
    *   `content_type_of_attachment`: The MIME type of the attachment.
    *   `size_of_attachment_in_bytes`: The size of the attachment in bytes.
    *   `attachment_name_stored_after_download`: The filename used to save the attachment in the `attachments` folder (e.g., `01_report.pdf`).

### Example `metadata.json`

```json
{
  "to": [
    {
      "emailAddress": {
        "name": "Jane Doe",
        "address": "jane.doe@example.com"
      }
    }
  ],
  "cc": [
    {
      "emailAddress": {
        "name": "John Smith",
        "address": "john.smith@example.com"
      }
    }
  ],
  "from": {
    "emailAddress": {
      "name": "Marketing Team",
      "address": "marketing@example.com"
    }
  },
  "subject": "Q3 Financial Report and Project Updates",
  "received_date": "2024-07-21T14:30:00Z",
  "body": "body.html",
  "content_type_of_body": "text/html",
  "attachment_counts": 3,
  "list_of_attachments": [
    {
      "attachment_name_in_message": "Q3_Financials.xlsx",
      "content_type_of_attachment": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
      "size_of_attachment_in_bytes": 123456,
      "attachment_name_stored_after_download": "01_Q3_Financials.xlsx"
    },
    {
      "attachment_name_in_message": "Project_Timeline.pdf",
      "content_type_of_attachment": "application/pdf",
      "size_of_attachment_in_bytes": 789012,
      "attachment_name_stored_after_download": "02_Project_Timeline.pdf"
    },
    {
      "attachment_name_in_message": "company_logo.png",
      "content_type_of_attachment": "image/png",
      "size_of_attachment_in_bytes": 34567,
      "attachment_name_stored_after_download": "03_company_logo.png"
    }
  ]
}
```

## Token Management

The application requires a valid JWT access token for the Microsoft Graph API. You must provide the token using **exactly one** of the following methods:

| Flag                | Environment Variable | Description                                           |
| ------------------- | -------------------- | ----------------------------------------------------- |
| `-token-string`     |                      | Pass the token directly as a string.                  |
| `-token-file`       |                      | Provide the path to a file containing the token.      |
| `-token-env`        | `JWT_TOKEN`          | Read the token from the `JWT_TOKEN` env var.          |

For added security, you can use the `-remove-token-file` flag to automatically delete the token file after the application finishes.

## Command-line Arguments

All configuration options can be controlled via command-line arguments. Any flag you set will override the corresponding value in the configuration file.

| Argument                        | Description                                                               | Required | Default |
| ------------------------------- | ------------------------------------------------------------------------- | -------- | ------- |
| **Required**                    |                                                                           |          |         |
| `-mailbox`                      | The email address of the mailbox to download. Can also be set in config.  | **Yes**  |         |
| `-workspace`                    | The absolute path for storing artifacts. Can also be set in config.       | **Yes**  |         |
| **Token (Choose one)**          | Source can be string, file or environment variable                        | **Yes**  |         |
| `-token-string`                 | JWT token as a string.                                                    |          |         |
| `-token-file`                   | Path to a file containing the JWT token.                                  |          |         |
| `-token-env`                    | Read JWT token from the `JWT_TOKEN` environment variable.                 |          | `false` |
| `-remove-token-file`            | Remove the token file after use.                                          | No       | `false` |
| **General**                     |                                                                           |          |         |
| `-config`                       | Path to a JSON configuration file.                                        | No       |         |
| `-debug`                        | Enable debug logging.                                                     | No       | `false` |
| `-healthcheck`                  | Perform a health check and exit.                                          | No       | `false` |
| `-message-details <folder>`     | When used with `-healthcheck`, displays message details for the specified folder. | No       |         |
| `-version`                      | Display the application version and exit.                                 | No       | `false` |
| **Processing & State**          |                                                                           |          |         |
| `-processing-mode`              | Processing mode: `full`, `incremental`, or `route`.                       | No       | `full`  |
| `-inbox-folder`                 | The source folder from which to process messages.                         | No       | `Inbox` |
| `-state`                        | Path to the state file for incremental processing.                        | No       |         |
| `-state-save-interval`          | Save state every N messages during a run.                                 | No       | `100`   |
| `-processed-folder`             | Destination folder for successful messages in `route` mode.               | No       | `Processed`|
| `-error-folder`                 | Destination folder for failed messages in `route` mode.                   | No       | `Error` |
| **Email Body Conversion**       |                                                                           |          |         |
| `-convert-body`                 | Conversion mode for email bodies: `none`, `text`, or `pdf`.               | No       | `none`  |
| `-chromium-path`                | Absolute path to the headless Chromium/Chrome binary (required for `pdf`).| No       |         |
| **Performance & Limits**        |                                                                           |          |         |
| `-parallel`                     | Maximum number of parallel workers.                                       | No       | `4`     |
| `-timeout`                      | HTTP client timeout in seconds.                                           | No       | `60`    |
| `-api-rate`                     | API calls per second for client-side rate limiting.                       | No       | `10`    |
| `-api-burst`                    | API burst capacity for client-side rate limiting.                         | No       | `10`    |
| `-bandwidth-limit-mbs`          | Bandwidth limit in MB/s for downloads (0 for disabled).                   | No       | `0.0`   |

## Configuration File

For a more permanent setup, you can use a JSON file (e.g., `config.json`) and pass its path via the `-config` flag. All options available as flags can be set in the config file.

### Example `config.json`

```json
{
  "mailboxName": "user@example.com",
  "workspacePath": "/path/to/your/output",
  "tokenString": "your-jwt-token-here",
  "debugLogging": false,
  "processingMode": "route",
  "stateFilePath": "/path/to/your/state.json",
  "inboxFolder": "Inbox",
  "processedFolder": "Processed-Archive",
  "errorFolder": "Error-Items",
  "httpClientTimeoutSeconds": 180,
  "largeAttachmentThresholdMB": 25,
  "chunkSizeMB": 10,
  "maxParallelDownloads": 15,
  "apiCallsPerSecond": 4.0,
  "apiBurst": 8,
  "stateSaveInterval": 50,
  "bandwidthLimitMBs": 100.0
}
```

### Configuration Directives

*   **Required**:
    *   `mailboxName`: (String) The email address of the mailbox to download.
    *   `workspacePath`: (String) The absolute path to a unique folder for storing downloaded artifacts.
*   **Token Management**:
    *   `tokenString`: (String) The JWT token.
    *   `tokenFile`: (String) Path to the token file.
    *   `tokenEnv`: (Boolean) Set to `true` to use the `JWT_TOKEN` environment variable.
    *   `removeTokenFile`: (Boolean) Set to `true` to delete the token file after use.
*   **General**:
    *   `debugLogging`: (Boolean) Enables debug-level logging.
    *   `processingMode`: (String) `full`, `incremental`, or `route`. In `route` mode, messages are moved after processing.
    *   `inboxFolder`: (String) The source folder to process messages from. Defaults to the main `Inbox`.
    *   `stateFilePath`: (String) Absolute path to the state file for incremental mode.
    *   `processedFolder`: (String) The destination folder for successfully processed messages in `route` mode.
    *   `errorFolder`: (String) The destination folder for messages that failed processing in `route` mode.
*   **HTTP and API**:
    *   `httpClientTimeoutSeconds`: (Integer) Timeout in seconds for HTTP requests.
    *   `maxParallelDownloads`: (Integer) The maximum number of concurrent workers.
    *   `apiCallsPerSecond`: (Float) The number of API calls allowed per second.
    *   `apiBurst`: (Integer) The burst capacity for the API rate limiter.
    *   `bandwidthLimitMBs`: (Float) The download speed limit in megabytes per second. `0` means disabled.
*   **State**:
    *   `stateSaveInterval`: (Integer) How often to save the state file during a run (number of messages).
*   **Email Body Conversion**:
    *   `convertBody`: (String) The conversion mode for email bodies. Can be `none` (no conversion, saves `.html`), `text` (converts to plain text, saves `.txt`), or `pdf` (converts to PDF, saves `.pdf`). Defaults to `none`.
    *   `chromiumPath`: (String) The absolute path to a headless Chromium or Google Chrome binary. This is **required** if `convertBody` is set to `pdf`.

### A Note on API Permissions

For maximum security, it is recommended to use an Azure App Registration with the principle of least privilege.
*   For download-only modes (`full`, `incremental`), the `Mail.Read` permission is sufficient.
*   For `route` mode, which moves emails, the `Mail.ReadWrite` permission is required.

## Examples

### 1. Basic Run with Token String

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-string "YOUR_ACCESS_TOKEN"
```

### 2. Incremental Run Using a Token File

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-file "/path/to/token.txt" \
    -remove-token-file \
    -processing-mode incremental \
    -state "/path/to/state.json"
```

### 3. High-Performance Run Using a Config File and Overrides

This example uses a `config.json` file but overrides the parallelism and rate limits with command-line flags.

```shell
./o365mbx \
    -config "/path/to/your/config.json" \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -parallel 20 \
    -api-rate 10.0 \
    -api-burst 20
```

### 4. Route Mode with Default Folders

This example processes all messages from the "Inbox", saves the artifacts, and moves the original messages to either a "Processed" or "Error" folder in the mailbox.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-env \
    -processing-mode route
```

### 5. Route Mode with Custom Folders

This example does the same as above, but moves the messages to custom-named folders.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-env \
    -processing-mode route \
    -processed-folder "Archive/Succeeded" \
    -error-folder "Archive/Failed"
```

### 6. High-Throughput Download

This example configures the application for maximum download speed by increasing the number of parallel workers and raising the API rate limits.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-file "/path/to/token.txt" \
    -parallel 30 \
    -api-rate 15.0 \
    -api-burst 30
```

### 7. Bandwidth-Limited Download

This example throttles the total download speed to 50 MB/s to avoid hitting API data egress limits during a very large-scale download.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-file "/path/to/token.txt" \
    -bandwidth-limit-mbs 50.0
```

### 8. Body Conversion to Plain Text

This example downloads all emails and converts their bodies from HTML to plain text.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-file "/path/to/token.txt" \
    -convert-body text
```

### 9. Health Check Examples

The `-healthcheck` flag provides powerful, read-only tools to inspect the mailbox without downloading any items.

#### Basic Health Check

This example runs a general health check on the mailbox, showing overall stats and a list of all folders, sorted by name.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -token-env \
    -healthcheck
```

**Example Output:**

```text
--- Mailbox Health Check ---
Mailbox: user@example.com
------------------------------
Total Messages: 1573
Total Folders: 14
Total Mailbox Size: 250.75 MB
------------------------------

--- Folder Statistics ---
Folder                 Items   Size (KB)
-------                -----   ---------
Archive                50      102400.00
Conversation History   2       50.20
Deleted Items          10      5120.00
Drafts                 1       10.50
Inbox                  25      81920.00
Junk Email             5       1024.00
Outbox                 0       0.00
... (and so on)
-------------------------
```

#### Message Details Health Check

This example extends the health check to show detailed information for all messages within a specific folder, such as the `Inbox`.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -token-env \
    -healthcheck \
    -message-details "Inbox"
```

**Example Output:**

```text
--- Message Details for Folder: Inbox ---
From                  To                      Date                Subject                                                                 Attachments  Total Size (KB)
----                  --                      ----                -------                                                                 -----------  ----------------
sender@corp.com       user@example.com        2024-08-01 10:30    Q3 Financial Report and Project Updates                                 2            1205.63
another@sender.org    user@example.com;...    2024-08-01 09:15    Important: Action Required for your account                             0            5.12
marketing@company.com user@example.com        2024-07-31 16:00    Weekly Newsletter - Check out our new features and updates for this week! 0            15.30
... (and so on)
-------------------------------------------------
```

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.
