# Garmin Connect Personal API (copied from https://developer.garmin.com/gc-developer-program/personal/personal-api, 2026-10-28)

Garmin Connect Personal API
To obtain your own personal API key to access your health and fitness data securely, follow these steps:
Go to the Personal API Key page while logged in to your
Garmin Connect Web account
Select Create Key
Read and Accept the Terms and Conditions
Choose an expiration period
Select Create
For your security, the key will only be displayed once so copy, save, and protect the key like a password
Select Done
Obtaining an access token
Your personal API key can be used to generate an access token that allows you to access Garmin Connect resources. To obtain an access token, call the Obtain Token endpoint and replace your-api-key with the API key you generated.
curl -X POST 'https://personal.connectapi.garmin.com/personalapitoken-service/personal-api/personal-api-key/token' -H 'Authorization: ApiKey [your-api-key]'
