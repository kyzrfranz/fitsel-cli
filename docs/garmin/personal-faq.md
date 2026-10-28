# Garmin Connect Personal API FAQ (copied from https://developer.garmin.com/gc-developer-program/personal/personal-faq/, 2026-10-28)

Garmin Connect Personal API FAQ
The Garmin Connect Personal API is available for personal use and is the quickest way to gain access to your data for use in personal applications.
General
What is the Garmin Connect Personal API?
The Garmin Connect Personal API allows Garmin customers to securely access their own Garmin Connect data using an API. It is intended for developers, hobbyists, data enthusiasts, and individuals who want to build personal applications using their own Garmin data.
Who is the Personal API designed for?
The Personal API is intended for individual Garmin users who want programmatic access to their own data. It is not intended for commercial applications serving multiple end users.
How is the Personal API different from the Garmin Connect Developer Program?
The Personal API provides access to an individual user's own data using a self-generated API key. The Garmin Connect Developer Program is designed for organizations building integrations for multiple users and includes commercial business APIs and enterprise options.
Getting Started
How do I obtain a Personal API key?
Go to the Personal API Key page while logged in to your
Garmin Connect Web account
Select Create Key
Read and Accept the Terms and Conditions
Choose an expiration period
Select Create
For your security, the key will only be displayed once so copy, save, and protect the key like a password
Select Done
Does my Personal API key expire?
When creating a personal API key, you can configure the key to expire after 30 days, 90 days, 180 days, or one year. We'll send you an email notification about a week before your key expires to remind you to generate a new key so your application maintains access to your data. The status and expiration date of your key can be viewed at any time on
Garmin Connect Web
.
Can I view my API key later?
No. For security reasons, the API key is only displayed once when it is created. If you lose the key, you must delete it and generate a new one.
Can I revoke or delete my API key?
Yes. You may delete your Personal API key at any time on
Garmin Connect Web
. Once deleted, the key and any access tokens associated with that key, will no longer be usable.
Authentication
How do I use my Personal API Key to authenticate with Connect APIs?
Garmin Connect APIs use OAuth2 access tokens for authentication so your Personal API key cannot be used directly. You first need to obtain a token generated from your key. To obtain a token, call the Personal API token endpoint passing your key in the Authorization header. The API will return an access token that can be used to call supported Garmin Connect APIs. Here is an example cURL command:
curl -X POST 'https://personal.connectapi.garmin.com/personalapitoken-service/personal-api/personal-api-key/token' -H 'Authorization: ApiKey [your-api-key]'
How long is my access token valid?
Access tokens are valid for 24 hours or until the Personal API key expires, whichever occurs first.
Are refresh tokens supported?
No, refresh tokens are not supported. You can simply obtain a new access token using the endpoint described above.
Development
How can I get started with development?
Developers should consult the
Personal API Swagger documentation
and GitHub resources at
https://github.com/garmin
.
Developer Blog
Garmin Brand Guidelines
Contact
Developer Forum
