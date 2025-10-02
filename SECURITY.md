# Security Policy

We are committed to ensuring the security of our application, and addressing security issues with a high priority.

## Supported Versions

We recommend always using the latest commit from the `main` branch, as we currently do not have a formal versioning scheme with designated security support.

## Reporting a Vulnerability

If you discover a security vulnerability, please report via the following methods:

1.  **Email** to `mis@criticalsys.net`. We will respond to your report within 48 hours.
2.  **GitHub Private Vulnerability Reporting**: If this feature is enabled for the repository, please use it to submit your report. This is the most secure and preferred method.
3.  **Create a Confidential Issue**: If private vulnerability reporting is not available, please create an issue on our GitHub repository. Please provide a clear and descriptive title, such as "Security Vulnerability: [Brief Description]", and include as much detail as possible in the issue description. If you have the option to make the issue confidential, please do so.

Please include the following information in your report:
- A clear description of the vulnerability.
- Steps to reproduce the vulnerability.
- The version of the application you are using.
- The potential impact of the vulnerability.
- Any suggested mitigations or fixes, if you have them.

We appreciate your efforts to responsibly disclose your findings, and we will make every effort to acknowledge your contributions.
We will make our best effort to respond to your report promptly, acknowledge the issue, and keep you updated on our progress toward a fix.
We kindly ask that you do not disclose the vulnerability publicly until we have had a chance to address it.

Please do not report security vulnerabilities through public GitHub issues nor PR.

Thank you for helping to keep our project secure.

---

## Application Security Measures

In addition to our vulnerability reporting policy, the application itself is built with security as a core principle. The following are key security features and practices embedded in the application's design:

### Secure Token Handling

*   **Mutually Exclusive Token Sources:** The application enforces that exactly one token source (`-token-string`, `-token-file`, or `-token-env`) can be used at a time. This prevents ambiguity and reduces the risk of accidentally exposing a token from an unintended source.
*   **Automatic Token File Removal:** The `-remove-token-file` flag provides a convenient way to ensure that the token file is automatically deleted from the disk after the application finishes its run, minimizing the exposure of credentials on the file system.

### Filesystem and Path Safety

*   **Workspace Validation:** Before any files are written, the application performs several critical checks on the provided workspace directory:
    *   It ensures the path is absolute, preventing ambiguity.
    *   It prohibits the use of critical system directories (e.g., `/`, `/etc`, `/root`) as a workspace to prevent accidental damage to the system.
    *   It verifies that the workspace path is a directory and not a symbolic link. This is a crucial defense against Time-of-Check-to-Time-of-Use (TOCTOU) attacks, where a symlink could be manipulated to cause the application to write files to an unintended location.
*   **Filename Sanitization:** All filenames from email attachments are rigorously sanitized before being saved to disk. This process removes characters that are invalid in file paths (e.g., `/`, `\`, `*`) and strips path traversal sequences (e.g., `..`) to prevent attackers from writing files outside of the intended workspace directory.
*   **Path Length Enforcement:** The application checks the total length of the final path for any file it creates. This prevents errors on filesystems with path length limitations (such as Windows) and mitigates potential denial-of-service vectors.

### Principle of Least Privilege

*   **API Permissions:** The documentation strongly recommends adhering to the principle of least privilege when configuring API permissions in Azure. For download-only modes, `Mail.Read` is sufficient, while the more permissive `Mail.ReadWrite` is only required for `route` mode, which moves emails. This minimizes the potential impact if an access token were to be compromised.
