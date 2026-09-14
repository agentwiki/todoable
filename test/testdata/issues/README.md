# 실제 이슈 수정 실행 환경

SC-28은 임시 Forgejo의 실제 이슈와 opened/reopened webhook을 받아 제품 CLI로 접수하고, 실제 Codex가 별도 Git 체크아웃을 수정한 결과를 종료검사로 확인한다. 계정·저장소·알림 대상은 테스트 내부에만 존재한다.

저장소 루트에서 `scripts/verify.sh --deep`으로 실행한다. Docker 서버, Go, Python 3, Git, 인증된 `codex-cli 0.154.0`이 필요하다. 모델 호출은 사용자의 기존 Codex 인증을 사용하며 실제 서비스 사용량이 발생한다. 일반 검증에서는 L3 실행 조건을 명시하고 건너뛰며 deep에서는 서비스·인증·실행 실패를 테스트 실패로 보고한다.

Forgejo 16.0.4 이미지는 `codeberg.org/forgejo/forgejo@sha256:a3e33d03e771d3e58b27de5573c3a25dc4f670583a6724c1878a6d0bbecf3556`으로 고정한다. 실제 버전 API 응답은 `16.0.4+gitea-1.22.0`이다. 컨테이너 웹 포트는 호스트의 127.0.0.1에만 게시한다. 임시 사용자와 비운영 저장소를 생성하고 메일러·등록·Actions·SSH는 비활성화한다. 실행 후 컨테이너와 볼륨을 제거한다. 이미지 캐시는 후속 검증을 위해 Docker에 남는다.

`receiver.go`는 컨테이너 내부 loopback에서 webhook을 수신하는 독립 테스트 실행기다. Go의 기존 testdata 제외 규칙에 따라 일반 제품 빌드 대상은 아니며, E2E가 정적 바이너리로 빌드하여 실제 Forgejo 사건을 수신하는 것으로 검증한다. `workflow.py`가 수신한 원본 payload와 delivery 식별자를 입력에 포함해 공개 CLI로 접수한다. 같은 사건 재전송은 접수·Run·Step·예산·모델 호출을 보존해야 한다.

`agent.py`는 사용자 설정과 규칙 파일을 불러오지 않는 Codex의 비대화형 실행으로 임시 체크아웃만 수정한다. `check.py`의 기대 결과는 모델 실행 전에 고정되어 있으며 모델이 쓰는 체크아웃 밖에 있다. 실제 모델 응답의 turn.completed와 토큰 사용 기록, 소스 변경, 독립 Python 실행 결과를 함께 확인한다. opened의 바이너리 크기 표기 요구를 만족한 뒤 reopened의 정밀도 인자 요구를 새 입력으로 처리한다.

실패 경계는 실제 모델 수정 뒤 어댑터가 정상 코드19로 종료하는 경우, 실제 모델을 리뷰만 수행하게 하여 종료조건을 충족하지 못하는 경우, 모델 완료 뒤 어댑터의 Step 결과 기록 전에 데몬을 강제 중단하는 경우다. 마지막 경우 재시작만으로 성공하거나 모델을 재호출하지 않고 불명 Step·로그·호출 예산을 보존해야 한다.

설치 방식은 [Forgejo 공식 Docker 안내](https://forgejo.org/docs/v16.0/admin/installation/docker/), 모델 실행 방식은 [공식 OpenAI 비대화형 Codex 안내](https://learn.chatgpt.com/docs/non-interactive-mode)를 따르며 실제 설치된 CLI 도움말과 서비스 API를 대조했다. 운영 Git 저장소나 사람에게는 이슈·메시지를 게시하지 않는다.
