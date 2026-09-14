# 실제 PNG 변환 자료

SC-29는 이 디렉터리의 고정 원본을 임시 디렉터리에 복사하여 실제 ImageMagick으로 WebP를 만들고 Pillow로 원본과 산출물을 독립 디코딩한다. 제품 CLI로 등록·접수한 뒤 데몬을 실행하므로 변환기만 시험하는 테스트가 아니다.

저장소 루트에서 `scripts/verify.sh --deep`으로 실행한다. `/usr/bin/magick`의 ImageMagick 7.1.1-43 Q16과 `/usr/bin/python3`의 Pillow 11.1.0이 필요하다. deep 실행에서 도구 부재·버전 불일치는 실패이며 일반 검증에서는 L3 실행 조건을 명시하고 건너뛴다. 네트워크나 외부 계정은 사용하지 않는다. 입력·산출물·DB·프로세스는 테스트 임시 디렉터리에서 생성하고 정리한다.

| 원본 | 설치 패키지 | 출처와 라이선스 기록 |
| --- | --- | --- |
| git-logo.png | git 1:2.47.3-0+deb13u1 | /usr/share/gitweb/static/git-logo.png, [GPL-2 고지](licenses/git-copyright) |
| htop.png | htop 3.4.1-5 | /usr/share/pixmaps/htop.png, [GPL-2+ 고지](licenses/htop-copyright) |
| pngtest.png | libpng-dev 1.6.48-1+deb13u5 | /usr/share/doc/libpng-dev/examples/pngtest.png, [libpng 고지](licenses/libpng-dev-copyright) |

[manifest.json](manifest.json)은 원본 파일 SHA256, 크기, 독립 디코딩한 RGBA SHA256을 고정한다. 원본·라이선스 고지는 설치된 패키지에서 변경 없이 복사했다. GPL 전문은 [COPYING-GPL-2](licenses/COPYING-GPL-2)에 보존한다.

`convert.py`는 실제 변환기의 stdin에 원본을 전달한다. 두 변환 프로세스를 동시에 관측할 수 있도록 원본 전달만 rendezvous 파일까지 지연한다. `check.py`는 완료 조건을 실제 원본과 비교하고, `oracle.py`는 엔진 응답을 기대값으로 사용하지 않고 고정된 픽셀 해시와 산출물을 대조한다. `webp:lossless=true`만으로는 투명 픽셀 아래 RGB가 소실될 수 있어 `webp:exact=true`도 사용한다.
