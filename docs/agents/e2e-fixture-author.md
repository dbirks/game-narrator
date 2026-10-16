# Instructions for autonomous Dolphin scenario authors

1. Confirm user provided ISO path and local use rights, pin Dolphin version and isolate `--user` dir. Record SHA256 of ISO **only**. Never copy ISO bytes into repository/artifacts.
2. Verify rendering with real screenshot and nonzero scene pixel entropy. Configure controller device with an observed accepted input. Save commandline, emulator config digest, rendered video backend and test machine info.
3. Progress through game with bounded explore time, saving normal in-game state regularly and temporary Dolphin savestates for test starts. Do not overwrite user's own Dolphin directories.
4. For a new interesting dialogue scene, record: start state pointer, input action timeline, screenshot sequence, textbox ROI, what raw text appears, original source message ID if imported, expected speaker/kind, expected audio action and any variable tokens. Do **not** copy copyrighted game text into public repo; keep private fixture bundles and synthetic public analogs.
5. Re-run controller sequence at least twice, validate checkpoints and ensure text is stable despite renderer timing. If nondeterministic, record robust triggers and tolerances instead of fixed real-time sleep durations.
6. Set provider mode explicitly (`recorded`, `live`) and use distinct test IDs. Recorded tests can use stored responses; live tests must capture raw route/model/request evidence with secrets removed.
7. Use Playwright to watch `/developer`, select the occurrence ID and independently verify frame/decision/voice/audio links and actual UI screenshot.
8. Force a failure case (skip, noisy frame, missing voice) and ensure it produces an intelligible failing HTML report. Add it to catalog and continuous regression.
9. Verify cleanup and re-run without deleting canonical game profile, source asset, approved voice or user save files.
10. Classify fixture as synthetic/shared or rights-limited/private and prevent copyrighted screenshots/savestates from entering public release.
