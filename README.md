# O365 Mailbox Downloader

`o365mbx` is a command-line application written in Go to download emails and attachments from Microsoft Office 365 (O365) mailboxes using the Microsoft Graph API.

It is designed to be robust and efficient, with features for handling large mailboxes, large attachments, and transient network errors.

## Features

*   **Email and Attachment Download**: Downloads emails and their attachments from a specified O365 mailbox.
*   **HTML to Plain Text Conversion**: Cleans email bodies by converting HTML to plain text, preserving links and image alt text.
*   **Incremental Downloads**: Performs incremental downloads by saving the timestamp of the last run, fetching only new emails since that time.
*   **Robust Error Handling**: Implements custom error types for better error identification and handling (Authentication, API, File System).
*   **Retry Mechanism**: Includes an exponential backoff retry mechanism for transient network errors and API rate limiting.
*   **Efficient Large Attachment Handling**: Uses chunked downloads for large attachments to avoid timeouts and reduce memory usage.
*   **Parallel Downloads**: Downloads multiple messages and attachments concurrently to speed up the process, with a configurable limit.
*   **Client-Side Rate Limiting**: Proactively manages the rate of API calls to avoid hitting server-side limits.
*   **Graceful Shutdown**: Supports graceful shutdown on interrupt signals (Ctrl+C), ensuring that ongoing operations are not abruptly terminated.
*   **Flexible Configuration**: Supports configuration via both a JSON file and command-line arguments, with arguments overriding file settings.
*   **Health Check Mode**: Provides a "health check" mode to verify connectivity and authentication with the O365 mailbox without performing a full download.
*   **Structured Logging**: Uses `logrus` for structured and informative logging.

## Command-line Arguments

The application can be configured using the following command-line arguments:

| Argument                       | Description                                                                    | Required | Default     |
|--------------------------------|--------------------------------------------------------------------------------|----------|-------------|
| `-token-string`                | Provides the JWT token directly as a string.                                   | **One of**   |             |
| `-token-file`                  | Path to a file containing the JWT token.                                       | **One of**   |             |
| `-token-env`                   | Reads the JWT token from the `JWT_TOKEN` environment variable.                 | **One of**   |             |
| `-remove-token-file`           | If using `-token-file`, remove the file after use.                             | No       | `false`     |
| `-mailbox`                     | The email address of the mailbox to download (e.g., `user@example.com`).       | **Yes**  |             |
| `-workspace`                   | The absolute path to a unique folder for storing downloaded artifacts.         | **Yes**  |             |
| `-config`                      | Path to a JSON configuration file.                                             | No       |             |
| `-processing-mode`             | The processing mode. One of `full`, `incremental`, or `route`.                 | No       | `route`     |
| `-processed-folder`            | In `route` mode, the folder to move successfully processed emails to.            | No       | `processed` |
| `-error-folder`                | In `route` mode, the folder to move emails that failed to process.               | No       | `error`     |
| `-timeout`                     | HTTP client timeout in seconds.                                                | No       | `120`       |
| `-parallel`                    | Maximum number of parallel downloads.                                          | No       | `10`        |
| `-max-retries`                 | Maximum number of retries for failed API calls.                                | No       | `5`         |
| `-initial-backoff-seconds`     | Initial backoff in seconds for retries.                                        | No       | `1`         |
| `-large-attachment-threshold-mb` | Threshold in MB for large attachments.                                         | No       | `20`        |
| `-chunk-size-mb`               | Chunk size in MB for large attachment downloads.                               | No       | `4`         |
| `-api-rate`                    | API calls per second for client-side rate limiting.                            | No       | `5.0`       |
| `-api-burst`                   | API burst capacity for client-side rate limiting.                              | No       | `10`        |
| `-dir-perms`                   | Permissions for created directories (e.g., `750`).                             | No       | `755`       |
| `-file-perms`                  | Permissions for created files (e.g., `640`).                                   | No       | `644`       |
| `-healthcheck`                 | Perform a health check on the mailbox and exit.                                | No       | `false`     |
| `-version`                     | Display the application version and exit.                                      | No       | `false`     |

## Configuration File

For more advanced configuration, you can use a JSON file (e.g., `config.json`) and pass its path via the `-config` flag. Command-line arguments will always override the values in the configuration file.

### Example `config.json`

A configuration file can specify any of the command-line arguments. Note that only one token source (`tokenString`, `tokenFile`, or `tokenEnv`) should be specified.

```json
{
  "tokenFile": "/path/to/secure/token.txt",
  "removeTokenFile": true,
  "mailboxName": "user@example.com",
  "workspacePath": "/path/to/your/output",
  "processingMode": "route",
  "processedFolder": "Processed-Emails",
  "errorFolder": "Failed-Emails",
  "httpClientTimeoutSeconds": 180,
  "maxRetries": 7,
  "initialBackoffSeconds": 2,
  "largeAttachmentThresholdMB": 50,
  "chunkSizeMB": 8,
  "maxParallelDownloads": 15,
  "apiCallsPerSecond": 3.0,
  "apiBurst": 6,
  "dirPerms": 750,
  "filePerms": 640
}
```

### Configuration Directives

*   `tokenString`: (String) The JWT token as a string.
*   `tokenFile`: (String) Path to a file containing the JWT token.
*   `tokenEnv`: (Boolean) If `true`, reads the token from the `JWT_TOKEN` environment variable.
*   `removeTokenFile`: (Boolean) If `true` and `tokenFile` is used, the token file will be deleted after use. Default: `false`.
*   `mailboxName`: (String) **Required**. The email address of the mailbox to download.
*   `workspacePath`: (String) **Required**. The absolute path to a unique folder for storing downloaded artifacts.
*   `processingMode`: (String) The processing mode. One of `full`, `incremental`, or `route`. Default: `route`.
*   `processedFolder`: (String) In `route` mode, the folder to move successfully processed emails to. Default: `processed`.
*   `errorFolder`: (String) In `route` mode, the folder to move emails that failed to process. Default: `error`.
*   `httpClientTimeoutSeconds`: (Integer) Timeout in seconds for HTTP requests. Default: `120`.
*   `maxRetries`: (Integer) Maximum number of retries for failed API requests. Default: `5`.
*   `initialBackoffSeconds`: (Integer) Initial backoff duration in seconds for the retry mechanism. Default: `1`.
*   `largeAttachmentThresholdMB`: (Integer) Attachments larger than this size (in MB) will be downloaded in chunks. Default: `20`.
*   `chunkSizeMB`: (Integer) The size (in MB) of each chunk for large attachment downloads. Default: `4`.
*   `maxParallelDownloads`: (Integer) The maximum number of messages to process concurrently. Default: `10`.
*   `apiCallsPerSecond`: (Float) The number of API calls allowed per second. Default: `5.0`.
*   `apiBurst`: (Integer) The burst capacity for the API rate limiter. Default: `10`.
*   `dirPerms`: (Integer) Permissions for created directories (in octal format). Default: `755`.
*   `filePerms`: (Integer) Permissions for created files (in octal format). Default: `644`.

## JSON Output

For each email processed, the application creates a JSON file in the workspace directory. The filename is the message ID of the email (e.g., `AAMkAG...=.json`). This file contains detailed information about the processed email.

### Example JSON Output

```json
{
  "to": [
    "recipient1@example.com"
  ],
  "from": "sender@example.com",
  "subject": "Project Update & Attachments",
  "receivedDate": "2023-10-27T10:30:00Z",
  "body": "Hello Team,\n\nPlease find the project update attached.\n\nBest regards",
  "attachments": [
    {
      "name": "Project-Plan.docx",
      "size": 12345,
      "downloadUrl": "Project-Plan.docx_AAMkAG...=_Project-Plan.docx"
    }
  ],
  "status": {
    "state": "success",
    "details": "Message processed successfully."
  }
}
```

### JSON Field Descriptions

*   `to`: (Array of Strings) A list of recipient email addresses.
*   `from`: (String) The sender's email address.
*   `subject`: (String) The subject line of the email.
*   `receivedDate`: (String) The date and time the email was received, in RFC3339 format.
*   `body`: (String) The cleaned, plain-text version of the email body.
*   `attachments`: (Array of Objects) A list of objects, where each object represents an attachment.
    *   `name`: (String) The original filename of the attachment.
    *   `size`: (Integer) The size of the attachment in bytes.
    *   `downloadUrl`: (String) The local filename of the downloaded attachment within the workspace. **Note:** This is not a web URL.
*   `status`: (Object) Contains the final processing status of the email.
    *   `state`: (String) The final state, either `success` or `error`.
    *   `details`: (String) A summary of the processing outcome. In case of an error, this will contain details about what went wrong.

## Examples

### 1. Running with Minimal Required Arguments

This example runs the application with only the essential arguments, providing the token as a string. All other settings will use their default values (e.g., `processing-mode route`).

```shell
./o365mbx -token-string "YOUR_ACCESS_TOKEN" -mailbox "user@example.com" -workspace "/path/to/your/output"
```

### 2. Running with All Command-Line Arguments

This example demonstrates using all available command-line flags to customize behavior, reading the token from a file and removing it after use.

```shell
./o365mbx -token-file "/path/to/token.txt" \
          -remove-token-file \
          -mailbox "user@example.com" \
          -workspace "/path/to/your/output" \
          -timeout 300 \
          -parallel 5 \
          -api-rate 2.5 \
          -api-burst 5
```

### 3. Running with a Configuration File

You can specify all required arguments in the `config.json` file. If a setting is defined in both the config file and as a command-line flag, the command-line flag will always take precedence.

If all required arguments (`accessToken`, `mailboxName`, `workspacePath`) are in the config file, you only need to provide the `-config` flag:
```shell
./o365mbx -config "/path/to/your/config.json"
```

You can still provide other flags to override specific settings in the file:
```shell
# This will use the settings from the config file, but override the number of parallel downloads.
./o365mbx -config "/path/to/your/config.json" -parallel 5
```

## Processing Modes

The application has three distinct processing modes, configured via the `-processing-mode` flag or the `processingMode` JSON key.

### `route` (Default)

This is the default mode. In `route` mode, the application will:
1.  Process all messages currently in the Inbox.
2.  Move successfully processed emails to the "processed" folder (or the folder specified by `-processed-folder`).
3.  Move emails that failed to process to the "error" folder (or the folder specified by `-error-folder`).

This mode is ideal for automated, continuous processing, as the Inbox is cleared of processed items after each run.

**Example:**
```shell
./o365mbx -token-string "YOUR_TOKEN" \
          -mailbox "user@example.com" \
          -workspace "/path/to/output" \
          -processing-mode route \
          -processed-folder "Archive-Success" \
          -error-folder "Archive-Failed"
```

### `incremental`

In `incremental` mode, the application will:
1.  Check the `last_run_timestamp.txt` file in the workspace to see when it last ran.
2.  Process only emails that have arrived *since* that timestamp.
3.  **Emails are not moved** from the Inbox.

This mode is useful for periodically downloading new emails without altering the state of the mailbox.

**Example:**
```shell
./o365mbx -token-string "YOUR_TOKEN" \
          -mailbox "user@example.com" \
          -workspace "/path/to/output" \
          -processing-mode incremental
```

### `full`

In `full` mode, the application will:
1.  Process **all** emails in the Inbox, regardless of when it last ran.
2.  **Emails are not moved** from the Inbox.

This mode is useful for creating a complete, one-time backup of the entire Inbox.

**Example:**
```shell
./o365mbx -token-string "YOUR_TOKEN" \
          -mailbox "user@example.com" \
          -workspace "/path/to/output" \
          -processing-mode full
```

## Health Check Mode

The health check mode allows you to quickly verify that the application can connect to the specified mailbox and that the access token is valid. It retrieves and displays the total message count in the inbox.

To run a health check, use the `-healthcheck` flag. You must still provide a token source.

```shell
./o365mbx -token-env -mailbox "user@example.com" -healthcheck
```

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.
